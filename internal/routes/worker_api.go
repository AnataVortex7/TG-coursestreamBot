package routes

import (
	"net/http"

	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	workerreg "EverythingSuckz/fsb/internal/worker"

	"github.com/gin-gonic/gin"
)

// LoadWorkerAPI registers the /api/worker/* routes.
// Called automatically by routes.Load() via reflection.
func (e *allRoutes) LoadWorkerAPI(r *Route) {
	// Self-registration — called by workers on startup (no auth needed)
	r.Engine.POST("/api/worker/register", handleWorkerRegister)

	// Heartbeat — called by workers every 25s (no auth needed)
	r.Engine.POST("/api/worker/heartbeat", handleWorkerHeartbeat)

	// Admin-only management endpoints
	r.Engine.GET("/api/worker/list", auth.RequireAuth, handleWorkerList)
	r.Engine.DELETE("/api/worker/remove", auth.RequireAuth, handleWorkerRemove)
}

// ── POST /api/worker/register ─────────────────────────────────────────────────

type workerRegisterRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleWorkerRegister(c *gin.Context) {
	var req workerRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "worker_url is required"})
		return
	}

	workerreg.RegisterWorker(req.WorkerURL)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── POST /api/worker/heartbeat ────────────────────────────────────────────────

type workerHeartbeatRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
	Status    string `json:"status"`
}

func handleWorkerHeartbeat(c *gin.Context) {
	var req workerHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "worker_url is required"})
		return
	}

	workerreg.HeartbeatWorker(req.WorkerURL)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── GET /api/worker/list  (admin only) ────────────────────────────────────────

func handleWorkerList(c *gin.Context) {
	workers, err := db.GetAllWorkers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}

	// Enrich with live in-memory health status
	healthMap := make(map[string]bool)
	for _, s := range workerreg.Status() {
		if url, ok := s["url"].(string); ok {
			healthMap[url] = s["healthy"].(bool)
		}
	}

	type workerView struct {
		URL      string `json:"url"`
		Status   string `json:"status"`
		Healthy  bool   `json:"healthy"`
		LastSeen string `json:"last_seen"`
		AddedAt  string `json:"added_at"`
	}

	out := make([]workerView, 0, len(workers))
	for _, w := range workers {
		out = append(out, workerView{
			URL:      w.URL,
			Status:   w.Status,
			Healthy:  healthMap[w.URL],
			LastSeen: w.LastSeen.Format("2006-01-02 15:04:05 UTC"),
			AddedAt:  w.AddedAt.Format("2006-01-02 15:04:05 UTC"),
		})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "workers": out})
}

// ── DELETE /api/worker/remove  (admin only) ───────────────────────────────────

type workerRemoveRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleWorkerRemove(c *gin.Context) {
	var req workerRemoveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "worker_url is required"})
		return
	}

	if err := workerreg.RemoveWorker(req.WorkerURL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
