// internal/routes/colab_upload.go
// ── Colab Upload Receive Endpoint ────────────────────────────────────────────
// Colab FFmpeg done → processed file Koyeb la POST karto
// Koyeb uploader ne upload → botClient MTProto ne send
//
// POST /api/colab/upload/:token
// Content-Type: application/octet-stream
// Body: processed video file bytes

package routes

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/gin-gonic/gin"
)

func (e *allRoutes) LoadColabUpload(r *Route) {
	r.Engine.POST("/api/colab/upload/:token", handleColabUpload)
}

// POST /api/colab/upload/:token
// Colab yethun processed file receive karo → temp file la save karo
// Job madhe uploadReady = true set karo → processSingleMessage upload karil
func handleColabUpload(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := c.Param("token") // token = job_id
	job, ok := wmqueue.Default.Get(jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.Status != wmqueue.StatusProcessing {
		c.JSON(http.StatusConflict, gin.H{"error": "job not in processing state"})
		return
	}

	ext := filepath.Ext(job.FileName)
	if ext == "" {
		ext = ".mp4"
	}
	tmpFile, err := os.CreateTemp("", "colab-out-*"+ext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "temp file create failed"})
		return
	}
	outPath := tmpFile.Name()

	// Stream body → temp file (no size limit — MTProto will handle 2GB)
	written, err := io.Copy(tmpFile, c.Request.Body)
	tmpFile.Close()
	if err != nil || written == 0 {
		os.Remove(outPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("file write failed: written=%d err=%v", written, err)})
		return
	}

	// OutputPath set karo — processSingleMessage wait karil aani upload karil
	wmqueue.Default.SetOutputPath(jobID, outPath)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"written": written,
	})
}
