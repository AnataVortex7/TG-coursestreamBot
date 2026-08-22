package commands

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	
	"EverythingSuckz/fsb/internal/wmqueue"
)

type DashboardTask struct {
	UserID     int64
	UserName   string
	Text       string
	LastUpdate time.Time
	IsDone     bool
}

type DashboardManager struct {
	mu           sync.Mutex
	tasks        map[string]*DashboardTask // key: "userID-jobID"
	activeMsgID  int
	adminChatID  int64
	lastEditTime time.Time
	msgCreatedAt time.Time
	editCount    int
	botToken     string
}

var GlobalDashboard = &DashboardManager{
	tasks: make(map[string]*DashboardTask),
}

func (d *DashboardManager) SetAdminChat(botToken string, chatID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.botToken = botToken
	d.adminChatID = chatID
}

func (d *DashboardManager) UpdateTask(taskKey string, userID int64, userName string, text string, isDone bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.tasks == nil {
		d.tasks = make(map[string]*DashboardTask)
	}

	task, exists := d.tasks[taskKey]
	if !exists {
		task = &DashboardTask{UserID: userID, UserName: userName}
		d.tasks[taskKey] = task
	}
	task.Text = text
	task.LastUpdate = time.Now()
	task.IsDone = isDone

	// Cleanup old done tasks (older than 1 hour to prevent memory leak)
	for k, t := range d.tasks {
		if t.IsDone && time.Since(t.LastUpdate) > 1*time.Hour {
			delete(d.tasks, k)
		}
	}

	// Trigger render asynchronously to avoid blocking
	go d.triggerRender()
}

func (d *DashboardManager) triggerRender() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.adminChatID == 0 || d.botToken == "" {
		return
	}

	// Throttle to 5 seconds
	if time.Since(d.lastEditTime) < 5*time.Second {
		return
	}

	text := d.buildText()
	
	needsNewMsg := false
	if d.activeMsgID == 0 {
		needsNewMsg = true
	} else if d.editCount >= 10 || time.Since(d.msgCreatedAt) >= 2*time.Minute {
		needsNewMsg = true
	}

	if needsNewMsg {
		if d.activeMsgID != 0 {
			wmqueue.DeleteTgMessage(d.botToken, d.adminChatID, int64(d.activeMsgID))
		}
		newID := wmqueue.SendTgMessage(d.botToken, d.adminChatID, text)
		if newID != 0 {
			d.activeMsgID = int(newID)
			d.lastEditTime = time.Now()
			d.msgCreatedAt = time.Now()
			d.editCount = 0
		}
	} else {
		success := wmqueue.EditTgMessage(d.botToken, d.adminChatID, d.activeMsgID, text)
		if success {
			d.lastEditTime = time.Now()
			d.editCount++
		} else {
			// FloodWait or deleted, send a new one
			wmqueue.DeleteTgMessage(d.botToken, d.adminChatID, int64(d.activeMsgID))
			newID := wmqueue.SendTgMessage(d.botToken, d.adminChatID, text)
			if newID != 0 {
				d.activeMsgID = int(newID)
				d.lastEditTime = time.Now()
				d.msgCreatedAt = time.Now()
				d.editCount = 0
			}
		}
	}
}
func (d *DashboardManager) buildText() string {
	var sb strings.Builder
	sb.WriteString("📊 **Parallel Server Dashboard** 📊\n\n")

	// Group by User
	userTasks := make(map[int64][]*DashboardTask)
	var users []int64
	var userNames = make(map[int64]string)

	for _, t := range d.tasks {
		if !t.IsDone || time.Since(t.LastUpdate) < 5*time.Minute {
			if _, ok := userTasks[t.UserID]; !ok {
				users = append(users, t.UserID)
				userNames[t.UserID] = t.UserName
			}
			userTasks[t.UserID] = append(userTasks[t.UserID], t)
		}
	}

	if len(users) == 0 {
		sb.WriteString("No active tasks.")
		return sb.String()
	}

	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })

	for _, uid := range users {
		sb.WriteString(fmt.Sprintf("👤 **%s (TG: %d)**\n", userNames[uid], uid))
		tasks := userTasks[uid]
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].LastUpdate.After(tasks[j].LastUpdate) })
		
		for _, t := range tasks {
			lines := strings.Split(t.Text, "\n")
			// Try to compact the text for dashboard
			compact := ""
						for _, l := range lines {
				l = strings.TrimSpace(l)
				if l != "" && !strings.Contains(l, "Msg:") && !strings.Contains(l, "📌") {
					compact += l + " | "
				}
			}
			if compact == "" {
				compact = strings.ReplaceAll(t.Text, "\n", " ")
			}
			if len(compact) > 200 {
				compact = compact[:197] + "..."
			}
			sb.WriteString(fmt.Sprintf("🔹 %s\n", strings.TrimSuffix(compact, " | ")))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("*(Updates every 5s)*")
	return sb.String()
}
