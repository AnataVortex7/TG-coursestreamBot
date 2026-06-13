// internal/routes/colab_api.go
// ── Colab Watermark Job Queue API ────────────────────────────────────────────
// Routes automatically loaded via reflection (LoadColabAPI method).
//
// GET  /api/colab/poll            — Colab poll: pending job dya
// POST /api/colab/progress        — Colab → progress message edit karo
// POST /api/colab/done            — Colab → job successful
// POST /api/colab/failed          — Colab → job failed
// GET  /api/colab/tempfile/:token — Colab → downloaded file serve karo

package routes

import (
	"fmt"
	"net/http"
	"os"

	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/gin-gonic/gin"
)

// LoadColabAPI — routes.go reflection ne automatically call karil
func (e *allRoutes) LoadColabAPI(r *Route) {
	r.Engine.GET("/api/colab/poll", handleColabPoll)
	r.Engine.POST("/api/colab/progress", handleColabProgress)
	r.Engine.POST("/api/colab/done", handleColabDone)
	r.Engine.POST("/api/colab/failed", handleColabFailed)
	r.Engine.GET("/api/colab/tempfile/:token", handleColabTempFile)
	r.Engine.GET("/api/colab/status", handleColabStatus)

	wmqueue.StartCleanupLoop()
	wmqueue.StartColabWatcher()
}

// ── Auth ──────────────────────────────────────────────────────────────────────

func colabSecretOK(c *gin.Context) bool {
	return wmqueue.SecretOK(c.GetHeader("X-Secret-Key"))
}

// ── GET /api/colab/poll ───────────────────────────────────────────────────────

func handleColabPoll(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	// Colab online tracker update karo
	wmqueue.RecordPoll()

	job := wmqueue.Default.Poll()
	if job == nil {
		c.JSON(http.StatusOK, gin.H{"job": nil, "stop": wmqueue.IsColabStopRequested()})
		return
	}
	// Colab ne processed file directly Telethon (MTProto, bot_token login) ne
	// target chat la pathvaycha aahe — job मध्ये send_to_chat_id / bot_token /
	// caption / etc already aahe (WatermarkJob struct). API_ID/API_HASH हे
	// Telethon client init साठी lagतात — env madhe je naava aahet tech vapra
	// (he placeholder names aahet, Go config package madhe check kara).
	c.JSON(http.StatusOK, gin.H{
		"job":      job,
		"api_id":   os.Getenv("API_ID"),
		"api_hash": os.Getenv("API_HASH"),
		"stop":     wmqueue.IsColabStopRequested(),
	})
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
	job, ok := wmqueue.Default.Get(req.JobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.ProgressMsgID > 0 {
		wmqueue.EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID, req.Message)
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
	wmqueue.Default.UpdateStatus(req.JobID, wmqueue.StatusDone, "")
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
	wmqueue.Default.UpdateStatus(req.JobID, wmqueue.StatusFailed, req.Error)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── GET /api/colab/tempfile/:token ────────────────────────────────────────────
// Pipe-first: userbot ne directly response madhe stream karto — disk nahi
// Fallback: temp file serve (old method, nahi vaparleli aata)

func handleColabTempFile(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	tkn := c.Param("token")

	// ── Pipe mode (userbot direct stream) ────────────────────────────────
	fileName, fileSize, pipeFn, ok := wmqueue.GetPipe(tkn)
	if ok {
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
		if fileSize > 0 {
			c.Header("Content-Length", fmt.Sprintf("%d", fileSize))
		}
		c.Header("Content-Type", "application/octet-stream")
		c.Status(http.StatusOK)
		if err := pipeFn(c.Writer); err != nil {
			// Pipe error — Colab download fail hoil, job failed madhe jaeil
			_ = err
		}
		return
	}

	// ── Fallback: temp file serve ─────────────────────────────────────────
	filePath, fileName2, ok2 := wmqueue.GetTempFile(tkn)
	if !ok2 {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found or expired"})
		return
	}
	info, err := os.Stat(filePath)
	if err != nil || info.Size() == 0 {
		c.JSON(http.StatusGone, gin.H{"error": "file no longer available"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName2))
	c.Header("Content-Length", fmt.Sprintf("%d", info.Size()))
	c.File(filePath)
}

// ── GET /api/colab/status ─────────────────────────────────────────────────────
// Colab online/offline status + pending job count

func handleColabStatus(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"online":        wmqueue.IsColabOnline(),
		"last_poll_ago": wmqueue.LastPollAgo(),
	})
}
