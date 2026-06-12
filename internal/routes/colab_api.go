// internal/routes/colab_api.go
// ── Colab Watermark Job Queue API ────────────────────────────────────────────
// Ngrok nahi — Colab Koyeb la poll karto, job gheun process karto.
//
// Routes (automatically loaded via reflection in routes.go):
//   GET  /api/colab/poll              — Colab poll: pending job dya
//   POST /api/colab/progress          — Colab → progress message edit karo
//   POST /api/colab/done              — Colab → job successful
//   POST /api/colab/failed            — Colab → job failed
//   GET  /api/colab/tempfile/:token   — Colab → temp file download karo
//
// ENV:
//   COLAB_SECRET_KEY  — Colab aani Koyeb madhe same value
//   KOYEB_URL         — https://sick-nikaniki-shubham8208989-6d7c863a.koyeb.app
//   BOT_TOKEN         — Telegram bot token (progress message edit saathi)

package routes

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

	"github.com/gin-gonic/gin"
)

// ── Job Status ────────────────────────────────────────────────────────────────

const (
	wmJobPending    = "pending"
	wmJobProcessing = "processing"
	wmJobDone       = "done"
	wmJobFailed     = "failed"
)

// ── WatermarkJob ──────────────────────────────────────────────────────────────

type WatermarkJob struct {
	JobID         string    `json:"job_id"`
	FileURL       string    `json:"file_url"`        // Colab tempfile endpoint URL
	FileName      string    `json:"file_name"`
	FileSize      int64     `json:"file_size"`
	WatermarkText string    `json:"watermark_text"`
	BotToken      string    `json:"bot_token"`
	ChatID        int64     `json:"chat_id"`         // Progress update pathavaycha admin chat
	MsgID         int       `json:"msg_id"`
	ProgressMsgID int       `json:"progress_msg_id"` // Edit karaycha message ID
	SendToChatID  int64     `json:"send_to_chat_id"`
	SendToTopicID int       `json:"send_to_topic_id,omitempty"`
	Caption       string    `json:"caption"`
	CreatedAt     time.Time `json:"created_at"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ── Job Queue (in-memory, thread-safe) ───────────────────────────────────────

type wmQueue struct {
	mu      sync.Mutex
	jobs    map[string]*WatermarkJob
	pending []string
}

var WMQueue = &wmQueue{
	jobs:    make(map[string]*WatermarkJob),
	pending: []string{},
}

func (q *wmQueue) enqueue(job *WatermarkJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job.Status = wmJobPending
	job.CreatedAt = time.Now()
	job.UpdatedAt = time.Now()
	q.jobs[job.JobID] = job
	q.pending = append(q.pending, job.JobID)
}

func (q *wmQueue) poll() *WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		job, ok := q.jobs[id]
		if !ok || job.Status != wmJobPending {
			continue
		}
		job.Status = wmJobProcessing
		job.UpdatedAt = time.Now()
		return job
	}
	return nil
}

func (q *wmQueue) updateStatus(jobID, status, errMsg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job, ok := q.jobs[jobID]; ok {
		job.Status = status
		job.Error = errMsg
		job.UpdatedAt = time.Now()
	}
}

func (q *wmQueue) get(jobID string) (*WatermarkJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[jobID]
	return job, ok
}

func (q *wmQueue) cleanup() {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := time.Now().Add(-2 * time.Hour)
	for id, job := range q.jobs {
		if job.UpdatedAt.Before(cutoff) {
			delete(q.jobs, id)
		}
	}
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

// ── Auth Helper ───────────────────────────────────────────────────────────────

func colabSecretOK(c *gin.Context) bool {
	secret := os.Getenv("COLAB_SECRET_KEY")
	if secret == "" {
		return false
	}
	return c.GetHeader("X-Secret-Key") == secret
}

// ── LoadColabAPI — automatically called by routes.go reflection ───────────────

func (e *allRoutes) LoadColabAPI(r *Route) {
	// Colab poll + callbacks
	r.Engine.GET("/api/colab/poll", handleColabPoll)
	r.Engine.POST("/api/colab/progress", handleColabProgress)
	r.Engine.POST("/api/colab/done", handleColabDone)
	r.Engine.POST("/api/colab/failed", handleColabFailed)

	// Temp file serve — Colab download karil
	r.Engine.GET("/api/colab/tempfile/:token", handleColabTempFile)

	// Background goroutines
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			WMQueue.cleanup()
			tfRegistry.mu.Lock()
			cutoff := time.Now().Add(-4 * time.Hour)
			for token, entry := range tfRegistry.files {
				if entry.RegisteredAt.Before(cutoff) {
					delete(tfRegistry.files, token)
				}
			}
			tfRegistry.mu.Unlock()
		}
	}()
}

// ── GET /api/colab/poll ───────────────────────────────────────────────────────

func handleColabPoll(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	job := WMQueue.poll()
	c.JSON(http.StatusOK, gin.H{"job": job}) // nil job → {"job": null}
}

// ── POST /api/colab/progress ──────────────────────────────────────────────────

func handleColabProgress(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		JobID   string `json:"job_id"`
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	job, ok := WMQueue.get(req.JobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.ProgressMsgID > 0 {
		editTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID, req.Message)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── POST /api/colab/done ──────────────────────────────────────────────────────

func handleColabDone(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		JobID string `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	WMQueue.updateStatus(req.JobID, wmJobDone, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── POST /api/colab/failed ────────────────────────────────────────────────────

func handleColabFailed(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req struct {
		JobID string `json:"job_id"`
		Error string `json:"error"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	WMQueue.updateStatus(req.JobID, wmJobFailed, req.Error)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── GET /api/colab/tempfile/:token ────────────────────────────────────────────

func handleColabTempFile(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	token := c.Param("token")
	tfRegistry.mu.Lock()
	entry, ok := tfRegistry.files[token]
	tfRegistry.mu.Unlock()

	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found or expired"})
		return
	}
	info, err := os.Stat(entry.FilePath)
	if err != nil || info.Size() == 0 {
		c.JSON(http.StatusGone, gin.H{"error": "file no longer available"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.FileName))
	c.Header("Content-Length", fmt.Sprintf("%d", info.Size()))
	c.File(entry.FilePath)
}

// ── editTgMessage — Bot API direct call ──────────────────────────────────────

func editTgMessage(botToken string, chatID int64, msgID int, text string) {
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

// ── SubmitWatermarkJob — admin_stream.go madhun call karo ────────────────────

func SubmitWatermarkJob(
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
	WMQueue.enqueue(job)
	return jobID, nil
}

// ── WaitForWatermarkJob — admin_stream.go madhe job complete hoparyant wait ──

func WaitForWatermarkJob(ctx context.Context, jobID string) (bool, string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	timeout := time.After(4 * time.Hour)
	for {
		select {
		case <-ctx.Done():
			return false, "context cancelled"
		case <-timeout:
			return false, "job timeout (4hr exceeded)"
		case <-ticker.C:
			job, ok := WMQueue.get(jobID)
			if !ok {
				return false, "job not found in queue"
			}
			switch job.Status {
			case wmJobDone:
				return true, ""
			case wmJobFailed:
				return false, job.Error
			}
		}
	}
}
