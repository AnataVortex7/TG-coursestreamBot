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
	WorkerID      string    `json:"-"` // job ज्या Colab worker ला assign झाला आहे
}

// ── Colab Worker Online Tracker (per-worker) ──────────────────────────────────
// Multiple Colab instances (same public notebook, same secret key) parallel
// live असू शकतात — प्रत्येकाचा स्वतःचा WORKER_ID (uuid, notebook restart वर
// नवीन) असतो. प्रत्येक poll मध्ये worker_id येतो — त्याचा lastPollTime track
// करतो. 20 sec (4 missed polls @ 5s) मध्ये poll आला नाही → तो worker offline.

const workerOfflineThreshold = 20 * time.Second

var colabTracker = struct {
	mu        sync.Mutex
	lastPoll  map[string]time.Time // workerID -> last poll time
}{
	lastPoll: make(map[string]time.Time),
}

// RecordPoll — workerID च्या poll चा timestamp नोंदवतो
func RecordPoll(workerID string) {
	if workerID == "" {
		return
	}
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	colabTracker.lastPoll[workerID] = time.Now()
}

// IsColabOnline — कुठलाही 1 worker तरी online आहे का (overall system status)
func IsColabOnline() bool {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	for _, t := range colabTracker.lastPoll {
		if time.Since(t) < workerOfflineThreshold {
			return true
		}
	}
	return false
}

// ActiveWorkers — सध्या online असलेले worker IDs
func ActiveWorkers() []string {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	var ids []string
	for id, t := range colabTracker.lastPoll {
		if time.Since(t) < workerOfflineThreshold {
			ids = append(ids, id)
		}
	}
	return ids
}

// LastPollAgo — सर्वात recent poll कधी झाला (कुठल्याही worker कडून)
func LastPollAgo() string {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	var latest time.Time
	for _, t := range colabTracker.lastPoll {
		if t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return "never"
	}
	d := time.Since(latest).Round(time.Second)
	return d.String()
}

// snapshotOnlineWorkers — सध्या online असलेल्या workers चा set (StartColabWatcher साठी)
func snapshotOnlineWorkers() map[string]bool {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	set := make(map[string]bool)
	for id, t := range colabTracker.lastPoll {
		if time.Since(t) < workerOfflineThreshold {
			set[id] = true
		}
	}
	return set
}

// forgetWorker — worker बराच वेळ offline राहिला तर tracker मधून काढतो (memory cleanup)
func forgetWorker(workerID string) {
	colabTracker.mu.Lock()
	defer colabTracker.mu.Unlock()
	delete(colabTracker.lastPoll, workerID)
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

func (q *Queue) Poll(workerID string) *WatermarkJob {
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
		job.WorkerID = workerID
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

// ── RequeueStaleProcessingForWorker ───────────────────────────────────────────
// एक specific Colab worker offline झाला — फक्त त्याच worker कडे assign झालेले
// "processing" jobs परत "pending" मध्ये टाकतो (इतर online workers चे जॉब्स
// untouched राहतात). Online झाल्यावर तेच job परत Poll() मधून मिळतील आणि
// त्याच ProgressMsgID पासून fresh सुरू होतील.

func (q *Queue) RequeueStaleProcessingForWorker(workerID string) []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var affected []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing && job.WorkerID == workerID {
			job.Status = StatusPending
			job.WorkerID = ""
			job.UpdatedAt = time.Now()
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

// ── ProcessingJobsForWorker ────────────────────────────────────────────────────
// specific workerID कडे assign असलेले "processing" jobs

func (q *Queue) ProcessingJobsForWorker(workerID string) []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var list []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing && job.WorkerID == workerID {
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
// प्रत्येक Colab worker (workerID) चं online/offline state independently track
// करतो (multiple Colab instances parallel live असू शकतात — public notebook +
// shared secret key मुळे):
//   - worker online → offline: त्या worker कडे assign असलेल्या processing jobs
//     चे progress messages "⏸️ Colab worker offline" असे edit करतो + ALLOWED_USERS
//     ला notify करतो.
//   - worker offline → online (किंवा नवीन worker पहिल्यांदा दिसला): त्या worker कडे
//     अडकलेले processing jobs (RequeueStaleProcessingForWorker) परत pending मध्ये
//     टाकतो, त्याच ProgressMsgID वर "🔄 fresh restart" edit करतो, आणि
//     ALLOWED_USERS ला notify करतो.
//
// हे bot च्या इतर कुठल्याही functionality ला disturb करत नाही — फक्त wmqueue
// state वर काम करणारा independent goroutine आहे.

func StartColabWatcher() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		previouslyOnline := make(map[string]bool)
		for range ticker.C {
			nowOnline := snapshotOnlineWorkers()

			// ── नवीन online झालेले / पहिल्यांदा दिसलेले workers ──────────────
			for id := range nowOnline {
				if !previouslyOnline[id] {
					affected := Default.RequeueStaleProcessingForWorker(id)
					for _, job := range affected {
						if job.ProgressMsgID > 0 {
							go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID,
								fmt.Sprintf("🔄 *Colab worker online झालं*\n📄 `%s`\n♻️ Job पुन्हा सुरुवातीपासून सुरू होतोय...", job.FileName))
						}
					}
					if len(affected) > 0 {
						notifyAllowedUsers(fmt.Sprintf("✅ *Colab worker online झालं* (`%s`)\n♻️ %d job(s) resume होत आहेत.", shortID(id), len(affected)))
					} else {
						notifyAllowedUsers(fmt.Sprintf("✅ *Colab worker online झालं* (`%s`)", shortID(id)))
					}
				}
			}

			// ── आता offline झालेले workers ───────────────────────────────────
			for id := range previouslyOnline {
				if !nowOnline[id] {
					processing := Default.ProcessingJobsForWorker(id)
					for _, job := range processing {
						if job.ProgressMsgID > 0 {
							go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID,
								fmt.Sprintf("⏸️ *Colab worker offline झालं*\n📄 `%s`\n🔄 दुसरा/तोच worker परत online झाल्यावर हा job आपोआप fresh सुरू होईल.", job.FileName))
						}
					}
					notifyAllowedUsers(fmt.Sprintf("⚠️ *Colab worker offline झालं* (`%s`)\nProcessing jobs परत online झाल्यावर fresh resume होतील.", shortID(id)))
					forgetWorker(id)
				}
			}

			previouslyOnline = nowOnline
		}
	}()
}

// shortID — लांब uuid worker ID पैकी फक्त सुरुवातीचे काही अक्षर दाखवण्यासाठी
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
