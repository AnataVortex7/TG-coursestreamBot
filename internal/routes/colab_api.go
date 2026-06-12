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

	wmqueue.StartCleanupLoop()
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
	job := wmqueue.Default.Poll()
	c.JSON(http.StatusOK, gin.H{"job": job})
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

func handleColabTempFile(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	token := c.Param("token")
	filePath, fileName, ok := wmqueue.GetTempFile(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found or expired"})
		return
	}
	info, err := os.Stat(filePath)
	if err != nil || info.Size() == 0 {
		c.JSON(http.StatusGone, gin.H{"error": "file no longer available"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	c.Header("Content-Length", fmt.Sprintf("%d", info.Size()))
	c.File(filePath)
}
