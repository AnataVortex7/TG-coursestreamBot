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
	// प्रत्येक Colab instance चा स्वतःचा unique worker_id (notebook restart
	// वर नवीन uuid) — multiple Colab instances parallel live असू शकतात,
	// प्रत्येकाचं online/offline state independently track करण्यासाठी.
	workerID := c.Query("worker_id")
	if workerID == "" {
		workerID = c.GetHeader("X-Worker-ID")
	}

	// Colab online tracker update karo (per-worker)
	wmqueue.RecordPoll(workerID)

	job := wmqueue.Default.Poll(workerID)
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
		JobID    string `json:"job_id"`
		Message  string `json:"message"`
		WorkerID string `json:"worker_id"`
		Seq      int64  `json:"seq"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	// FFmpeg chalu astana /api/colab/poll call hot nahi (blocking),
	// pan /api/colab/progress dar 3-4 sec yet rahto — tyala pan
	// "alive" heartbeat mhanun count karo, taki worker false-offline
	// na disel.
	workerID := req.WorkerID
	if workerID == "" {
		workerID = c.Query("worker_id")
	}
	if workerID == "" {
		workerID = c.GetHeader("X-Worker-ID")
	}
	wmqueue.RecordPoll(workerID)

	job, ok := wmqueue.Default.Get(req.JobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}

	// [NEW] Telegram edit (खाली) फक्त ProgressMsgID>0 असेल तरच होतो — पण
	// stage/percent text इथेच नेहमी साठवतो, जेणेकरून ProgressMsgID==0 असलेल्या
	// jobs साठी (उदा. silent /predownload) सुद्धा कुणीतरी नंतर वाचू शकेल.
	// Retry चालू असेल तर "❌ Download failed" ऐवजी retry notice दाखवा
	req.Message = wmqueue.Default.AdjustProgress(req.JobID, req.Message)

	if job.MsgID > 0 {
		req.Message += fmt.Sprintf("\n📌 Msg: `%d`", job.MsgID)
	}
	wmqueue.SetProgressMessage(req.JobID, req.Message)

	// ── Ordered, serialized edit ─────────────────────────────────────────
	// seq <= last-applied → stale/out-of-order, skip.
	// आधीचा edit अजून चालू असेल किंवा spacing/backoff gap संपला नसेल →
	// हा skip (पुढचा update latest % दाखवेल). Telegram edit complete
	// झाल्यावरच (यशस्वी असो वा नसो) पुढचा pick होतो (FinishProgress).
	// [FIX] EditTgMessage आता actual success/failure return करतो — तो
	// FinishProgress ला पास करतो, जेणेकरून सलग failures (उदा. FLOOD_WAIT)
	// वर wmqueue आपोआप backoff gap वाढवेल, प्रत्येक tick ला निष्फळ प्रयत्न
	// होणार नाही.
	if job.ProgressMsgID > 0 {
		j, proceed := wmqueue.Default.TryProgress(req.JobID, req.Seq)
		if proceed {
			success := wmqueue.EditTgMessage(j.BotToken, j.ChatID, j.ProgressMsgID, req.Message)
			wmqueue.Default.FinishProgress(req.JobID, success)
		}
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
		JobID           string `json:"job_id"`
		WorkerID        string `json:"worker_id"`
		SentMessageID   int    `json:"sent_message_id"`    // ← नवीन, optional — जुना Colab script पाठवत नसेल तर 0
		LogChannelMsgID int    `json:"log_channel_msg_id"` // ← worker ने LOG_CHANNEL ला पाठवलेल्या master copy चा msg ID (DeliveredCache fast-path साठी, optional — नसेल तर 0)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	// [FIX] done call sudha heartbeat mhanun count karo — upload step la time
	// lagla aani tya darmyan progress na aalyas worker khota offline na disel.
	workerID := req.WorkerID
	if workerID == "" {
		workerID = c.Query("worker_id")
	}
	if workerID == "" {
		workerID = c.GetHeader("X-Worker-ID")
	}
	wmqueue.RecordPoll(workerID)

	if req.SentMessageID > 0 {
		wmqueue.Default.SetSentMessageID(req.JobID, req.SentMessageID)
	}
	if req.LogChannelMsgID > 0 {
		wmqueue.Default.SetLogChannelMsgID(req.JobID, req.LogChannelMsgID)
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
		JobID    string `json:"job_id"`
		Error    string `json:"error"`
		WorkerID string `json:"worker_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	workerID := req.WorkerID
	if workerID == "" {
		workerID = c.Query("worker_id")
	}
	if workerID == "" {
		workerID = c.GetHeader("X-Worker-ID")
	}
	wmqueue.RecordPoll(workerID)

	// Network timeout सारखी तात्पुरती failure → job failed न करता auto-retry
	if wmqueue.Default.RetryIfTransient(req.JobID, req.Error) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "retrying": true})
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
