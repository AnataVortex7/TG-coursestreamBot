// internal/routes/colab_upload.go
// ── Colab Chunked Upload Receive ──────────────────────────────────────────────
//
// Colab FFmpeg done → processed file 50MB chunks मध्ये Koyeb ला POST करतो
// Cloudflare 100MB limit bypass → chunks reassemble → MTProto send
//
// POST /api/colab/upload/:token/chunk/:index   — एक chunk receive करो
// POST /api/colab/upload/:token/finalize       — सगळे chunks reassemble करो
//
// जुना single-POST endpoint काढला — तोच 413 cause होता

package routes

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/gin-gonic/gin"
)

func (e *allRoutes) LoadColabUpload(r *Route) {
	// Chunked upload — 50MB chunks, Cloudflare safe
	r.Engine.POST("/api/colab/upload/:token/chunk/:index", handleColabChunk)
	r.Engine.POST("/api/colab/upload/:token/finalize", handleColabFinalize)
}

// ── POST /api/colab/upload/:token/chunk/:index ────────────────────────────────
// Colab एक chunk POST करतो — /tmp/colab-chunks-<jobID>/<index>.chunk ला save करतो
// index: 0, 1, 2, ... (Colab same order मध्ये pathavnar)

func handleColabChunk(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := c.Param("token")
	indexStr := c.Param("index")

	chunkIndex, err := strconv.Atoi(indexStr)
	if err != nil || chunkIndex < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk index"})
		return
	}

	job, ok := wmqueue.Default.Get(jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.Status != wmqueue.StatusProcessing {
		c.JSON(http.StatusConflict, gin.H{"error": "job not in processing state"})
		return
	}

	// Chunk dir — पहिल्या chunk वर create करो
	chunkDir, _, hasDir := wmqueue.Default.GetChunkInfo(jobID)
	if !hasDir {
		chunkDir = fmt.Sprintf("/tmp/colab-chunks-%s", jobID)
		if mkErr := os.MkdirAll(chunkDir, 0755); mkErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "chunk dir create failed"})
			return
		}
		// TotalChunks आत्ता माहीत नाही — finalize वर set होईल
		wmqueue.Default.SetChunkDir(jobID, chunkDir, 0)
	}

	// Chunk file save करो
	chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", chunkIndex))
	chunkFile, err := os.Create(chunkPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "chunk file create failed"})
		return
	}

	written, err := io.Copy(chunkFile, c.Request.Body)
	chunkFile.Close()
	if err != nil || written == 0 {
		os.Remove(chunkPath)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("chunk write failed: written=%d err=%v", written, err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"index":   chunkIndex,
		"written": written,
	})
}

// ── POST /api/colab/upload/:token/finalize ────────────────────────────────────
// सगळे chunks आले → reassemble करो → single output file → SetOutputPath
// Body JSON: {"total_chunks": N}

func handleColabFinalize(c *gin.Context) {
	if !colabSecretOK(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := c.Param("token")

	var req struct {
		TotalChunks int `json:"total_chunks"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.TotalChunks <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid total_chunks"})
		return
	}

	job, ok := wmqueue.Default.Get(jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.Status != wmqueue.StatusProcessing {
		c.JSON(http.StatusConflict, gin.H{"error": "job not in processing state"})
		return
	}

	chunkDir, _, hasDir := wmqueue.Default.GetChunkInfo(jobID)
	if !hasDir || chunkDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no chunks received yet"})
		return
	}
	defer os.RemoveAll(chunkDir) // cleanup chunks after reassemble

	// Available chunks list करो आणि sort करो
	entries, err := os.ReadDir(chunkDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "chunk dir read failed"})
		return
	}

	var chunkFiles []string
	for _, e := range entries {
		if !e.IsDir() {
			chunkFiles = append(chunkFiles, filepath.Join(chunkDir, e.Name()))
		}
	}
	sort.Strings(chunkFiles) // 00000.chunk, 00001.chunk, ... order

	if len(chunkFiles) != req.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("chunk count mismatch: got %d, expected %d",
				len(chunkFiles), req.TotalChunks),
		})
		return
	}

	// Output temp file create करो
	ext := filepath.Ext(job.FileName)
	if ext == "" {
		ext = ".mp4"
	}
	outFile, err := os.CreateTemp("", "colab-out-*"+ext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "output file create failed"})
		return
	}
	outPath := outFile.Name()

	// Chunks reassemble — sequential write
	var totalWritten int64
	for _, chunkPath := range chunkFiles {
		cf, openErr := os.Open(chunkPath)
		if openErr != nil {
			outFile.Close()
			os.Remove(outPath)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("chunk open failed: %s", openErr.Error()),
			})
			return
		}
		n, copyErr := io.Copy(outFile, cf)
		cf.Close()
		if copyErr != nil {
			outFile.Close()
			os.Remove(outPath)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("chunk reassemble failed: %s", copyErr.Error()),
			})
			return
		}
		totalWritten += n
	}
	outFile.Close()

	if totalWritten == 0 {
		os.Remove(outPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reassembled file is empty"})
		return
	}

	// OutputPath set करो — WaitForJob → processSingleMessage upload करेल
	wmqueue.Default.SetOutputPath(jobID, outPath)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"written": totalWritten,
		"chunks":  req.TotalChunks,
	})
}
