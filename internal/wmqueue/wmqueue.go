// internal/wmqueue/wmqueue.go
// ── Watermark Job Queue ───────────────────────────────────────────────────────
// Separate package — commands aani routes dono import karto, cycle nahi.
//
// commands → wmqueue  ✅
// routes   → wmqueue  ✅
// wmqueue  → (only stdlib + gin)  ✅

package wmqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
)

// ── Job Status ────────────────────────────────────────────────────────────────

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusFailed     = "failed"
)

// ── WatermarkJob ──────────────────────────────────────────────────────────────

type WatermarkJob struct {
	JobID         string    `json:"job_id"`
	FileURL       string    `json:"file_url"`
	FileName      string    `json:"file_name"`
	FileSize      int64     `json:"file_size"`
	WatermarkText string    `json:"watermark_text"`
	BotToken      string    `json:"bot_token"`
	ChatID        int64     `json:"chat_id"`
	MsgID         int       `json:"msg_id"`
	ProgressMsgID int       `json:"progress_msg_id"`
	SendToChatID  int64     `json:"send_to_chat_id"`
	SendToTopicID int       `json:"send_to_topic_id,omitempty"`
	Caption       string    `json:"caption"`
	CreatedAt     time.Time `json:"created_at"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ── Colab Online Tracker ──────────────────────────────────────────────────────

var colabTracker = struct {
	mu           sync.Mutex
	lastPollTime time.Time
}{
	lastPollTime: time.Time{},
}

func RecordPoll() {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	colabTracker.lastPollTime = time.Now()
}

func IsColabOnline() bool {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	if colabTracker.lastPollTime.IsZero() {
		return false
	}
	return time.Since(colabTracker.lastPollTime) < 2*time.Minute
}

func LastPollAgo() string {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	if colabTracker.lastPollTime.IsZero() {
		return "never"
	}
	d := time.Since(colabTracker.lastPollTime).Round(time.Second)
	return d.String()
}

// ── Colab Stop Flag ────────────────────────────────────────────────────────────
// Admin command (/colabstop) ने set होतो — Colab worker पुढच्या poll वर
// `stop: true` बघून loop graceful exit करतो. बाकी bot/server normal चालतो,
// फक्त Colab polling loop थांबतो.

var colabStop = struct {
	mu      sync.Mutex
	stopped bool
}{}

func RequestColabStop() {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	colabStop.stopped = true
}

func ClearColabStop() {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	colabStop.stopped = false
}

func IsColabStopRequested() bool {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	return colabStop.stopped
}

// ── Job Queue ─────────────────────────────────────────────────────────────────

type Queue struct {
	mu      sync.Mutex
	jobs    map[string]*WatermarkJob
	pending []string
}

var Default = &Queue{
	jobs:    make(map[string]*WatermarkJob),
	pending: []string{},
}

func (q *Queue) Enqueue(job *WatermarkJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job.Status = StatusPending
	job.CreatedAt = time.Now()
	job.UpdatedAt = time.Now()
	q.jobs[job.JobID] = job
	q.pending = append(q.pending, job.JobID)
}

func (q *Queue) Poll() *WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		job, ok := q.jobs[id]
		if !ok || job.Status != StatusPending {
			continue
		}
		job.Status = StatusProcessing
		job.UpdatedAt = time.Now()
		return job
	}
	return nil
}

func (q *Queue) UpdateStatus(jobID, status, errMsg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job, ok := q.jobs[jobID]; ok {
		job.Status = status
		job.Error = errMsg
		job.UpdatedAt = time.Now()
	}
}

func (q *Queue) Get(jobID string) (*WatermarkJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[jobID]
	return job, ok
}

// (OutputPath / ChunkDir helpers काढले — Colab आता थेट Telethon ने
//  Telegram la send करतो, Koyeb ला processed file परत येत नाही.)

func (q *Queue) Cleanup() {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := time.Now().Add(-2 * time.Hour)
	for id, job := range q.jobs {
		if job.UpdatedAt.Before(cutoff) {
			delete(q.jobs, id)
			continue
		}
		// Time-limit नाही — Colab offline असेल तर StartColabWatcher
		// processing jobs ला परत pending करतो (online झाल्यावर fresh resume).
	}
}

// ── RequeueStaleProcessing ────────────────────────────────────────────────────
// Colab offline असताना "processing" status मध्ये अडकलेले jobs परत "pending"
// मध्ये टाकतो — Colab online झाल्यावर तेच job परत Poll() मधून मिळतील आणि
// त्याच ProgressMsgID पासून fresh सुरू होतील (download+ffmpeg+upload पूर्ण नवीन,
// कारण आधीचं output incomplete असतं).

func (q *Queue) RequeueStaleProcessing() []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var affected []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing {
			job.Status = StatusPending
			job.UpdatedAt = time.Now()
			// प्राधान्याने सर्वात आधी poll व्हावा म्हणून queue च्या पुढे टाकतो
			q.pending = append([]string{job.JobID}, q.pending...)
			affected = append(affected, job)
		}
	}
	return affected
}

// ── ProcessingJobs ───────────────────────────────────────────────────────────
// सध्या "processing" status मधले सगळे jobs (notification साठी)

func (q *Queue) ProcessingJobs() []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var list []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing {
			list = append(list, job)
		}
	}
	return list
}

// ── Temp File Registry ────────────────────────────────────────────────────────

type tempFileEntry struct {
	FilePath     string
	FileName     string
	RegisteredAt time.Time
}

var tfRegistry = struct {
	mu    sync.Mutex
	files map[string]*tempFileEntry
}{
	files: make(map[string]*tempFileEntry),
}

func RegisterTempFile(token, filePath, fileName string) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	tfRegistry.files[token] = &tempFileEntry{
		FilePath:     filePath,
		FileName:     fileName,
		RegisteredAt: time.Now(),
	}
}

func UnregisterTempFile(token string) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	delete(tfRegistry.files, token)
}

func GetTempFile(token string) (filePath, fileName string, ok bool) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	entry, found := tfRegistry.files[token]
	if !found {
		return "", "", false
	}
	return entry.FilePath, entry.FileName, true
}

func CleanupTempFiles() {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	cutoff := time.Now().Add(-4 * time.Hour)
	for token, entry := range tfRegistry.files {
		if entry.RegisteredAt.Before(cutoff) {
			delete(tfRegistry.files, token)
		}
	}
}

// ── Pipe Registry ─────────────────────────────────────────────────────────────

type PipeFn func(w http.ResponseWriter) error

type pipeEntry struct {
	Fn           PipeFn
	FileName     string
	FileSize     int64
	RegisteredAt time.Time
}

var pipeRegistry = struct {
	mu    sync.Mutex
	pipes map[string]*pipeEntry
}{
	pipes: make(map[string]*pipeEntry),
}

func RegisterPipe(token, fileName string, fileSize int64, fn PipeFn) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	pipeRegistry.pipes[token] = &pipeEntry{
		Fn:           fn,
		FileName:     fileName,
		FileSize:     fileSize,
		RegisteredAt: time.Now(),
	}
}

func UnregisterPipe(token string) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	delete(pipeRegistry.pipes, token)
}

func GetPipe(token string) (fileName string, fileSize int64, fn PipeFn, ok bool) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	entry, found := pipeRegistry.pipes[token]
	if !found {
		return "", 0, nil, false
	}
	return entry.FileName, entry.FileSize, entry.Fn, true
}

func CleanupPipes() {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	cutoff := time.Now().Add(-4 * time.Hour)
	for token, entry := range pipeRegistry.pipes {
		if entry.RegisteredAt.Before(cutoff) {
			delete(pipeRegistry.pipes, token)
		}
	}
}

// ── SubmitJob ─────────────────────────────────────────────────────────────────

func SubmitJob(
	fileURL, fileName string,
	fileSize int64,
	wmText, botToken string,
	adminChatID int64,
	progressMsgID int,
	sendToChatID int64,
	sendToTopicID int,
	caption string,
	srcMsgID int,
) (string, error) {
	jobID := fmt.Sprintf("wm-%d-%d", time.Now().UnixNano(), rand.Intn(9999))
	job := &WatermarkJob{
		JobID:         jobID,
		FileURL:       fileURL,
		FileName:      fileName,
		FileSize:      fileSize,
		WatermarkText: wmText,
		BotToken:      botToken,
		ChatID:        adminChatID,
		MsgID:         srcMsgID,
		ProgressMsgID: progressMsgID,
		SendToChatID:  sendToChatID,
		SendToTopicID: sendToTopicID,
		Caption:       caption,
	}
	Default.Enqueue(job)
	return jobID, nil
}

// ── WaitForDone ───────────────────────────────────────────────────────────────
// Colab job poll karto, FFmpeg karto, aani स्वतः Telethon ने Telegram la
// processed file pathvतो. Koyeb फक्त job चा final status (done/failed) wait
// karto — output file Koyeb kade kadhich yet nahi.

func WaitForDone(ctx context.Context, jobID string) (bool, string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, "context cancelled"
		case <-ticker.C:
			job, ok := Default.Get(jobID)
			if !ok {
				return false, "job not found in queue"
			}
			switch job.Status {
			case StatusDone:
				return true, ""
			case StatusFailed:
				return false, job.Error
			}
		}
	}
}

// ── EditTgMessage ─────────────────────────────────────────────────────────────

func EditTgMessage(botToken string, chatID int64, msgID int, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

// ── SecretOK ──────────────────────────────────────────────────────────────────

func SecretOK(headerVal string) bool {
	secret := os.Getenv("COLAB_SECRET_KEY")
	if secret == "" {
		return false
	}
	return headerVal == secret
}

// ── StartCleanupLoop ──────────────────────────────────────────────────────────

func StartCleanupLoop() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			Default.Cleanup()
			CleanupTempFiles()
			CleanupPipes()
		}
	}()
}

// ── notifyAllowedUsers ─────────────────────────────────────────────────────────
// ALLOWED_USERS (config) मधल्या प्रत्येक user ला message पाठवतो — Colab
// online/offline notifications साठी.

func notifyAllowedUsers(text string) {
	botToken := config.ValueOf.BotToken
	for _, uid := range config.ValueOf.AllowedUsers {
		go sendTgMessage(botToken, uid, text)
	}
}

func sendTgMessage(botToken string, chatID int64, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

// ── StartColabWatcher ─────────────────────────────────────────────────────────
// Colab च्या online/offline state मध्ये बदल track करतो:
//   - online → offline: processing jobs चे progress messages "⏸️ Colab offline"
//     असे edit करतो + ALLOWED_USERS ला notify करतो.
//   - offline → online: processing jobs परत pending मध्ये (RequeueStaleProcessing)
//     टाकतो, त्याच ProgressMsgID वर "🔄 Colab online — fresh restart" edit करतो,
//     आणि ALLOWED_USERS ला notify करतो.
//
// हे bot च्या इतर कुठल्याही functionality ला disturb करत नाही — फक्त wmqueue
// state वर काम करणारा independent goroutine आहे.

func StartColabWatcher() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		wasOnline := false
		first := true
		for range ticker.C {
			online := IsColabOnline()
			if first {
				wasOnline = online
				first = false
				continue
			}
			if wasOnline && !online {
				// ── online → offline ──────────────────────────────────────
				for _, job := range Default.ProcessingJobs() {
					if job.ProgressMsgID > 0 {
						go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID,
							fmt.Sprintf("⏸️ *Colab offline झालं*\n📄 `%s`\n🔄 Colab परत online झाल्यावर हा job आपोआप fresh सुरू होईल — काही करण्याची गरज नाही.", job.FileName))
					}
				}
				notifyAllowedUsers("⚠️ *Colab offline झालं*\nProcessing jobs परत online झाल्यावर fresh resume होतील.")
			} else if !wasOnline && online {
				// ── offline → online ──────────────────────────────────────
				affected := Default.RequeueStaleProcessing()
				for _, job := range affected {
					if job.ProgressMsgID > 0 {
						go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID,
							fmt.Sprintf("🔄 *Colab online झालं*\n📄 `%s`\n♻️ Job पुन्हा सुरुवातीपासून सुरू होतोय...", job.FileName))
					}
				}
				notifyAllowedUsers(fmt.Sprintf("✅ *Colab online झालं*\n♻️ %d job(s) resume होत आहेत.", len(affected)))
			}
			wasOnline = online
		}
	}()
}
