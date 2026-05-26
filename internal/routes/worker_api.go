package routes

import (
	"crypto/subtle"
	"net/http"
	"os"

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

	// Admin manually adds a worker URL via admin panel
	r.Engine.POST("/api/worker/add", auth.RequireAuth, handleWorkerAdd)
}

// ── POST /api/worker/register ─────────────────────────────────────────────────

type workerRegisterRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleWorkerRegister(c *gin.Context) {
	secret := c.GetHeader("X-Worker-Secret")
	expected := os.Getenv("STREAM_SECRET")
	if subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return
	}

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
	secret := c.GetHeader("X-Worker-Secret")
	expected := os.Getenv("STREAM_SECRET")
	if subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return
	}

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

	// Enrich with live in-memory status (healthy + sleeping)
	type liveInfo struct {
		healthy  bool
		sleeping bool
		addedBy  string
	}
	liveMap := make(map[string]liveInfo)
	for _, s := range workerreg.Status() {
		url, _ := s["url"].(string)
		healthy, _ := s["healthy"].(bool)
		sleeping, _ := s["sleeping"].(bool)
		addedBy, _ := s["added_by"].(string)
		liveMap[url] = liveInfo{healthy: healthy, sleeping: sleeping, addedBy: addedBy}
	}

	type workerView struct {
		URL      string `json:"url"`
		Status   string `json:"status"`
		AddedBy  string `json:"added_by"`
		Healthy  bool   `json:"healthy"`
		Sleeping bool   `json:"sleeping"`
		LastSeen string `json:"last_seen"`
		AddedAt  string `json:"added_at"`
	}

	out := make([]workerView, 0, len(workers))
	for _, w := range workers {
		live := liveMap[w.URL]
		addedBy := w.AddedBy
		if addedBy == "" {
			addedBy = live.addedBy
		}
		out = append(out, workerView{
			URL:      w.URL,
			Status:   w.Status,
			AddedBy:  addedBy,
			Healthy:  live.healthy,
			Sleeping: live.sleeping,
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

// ── POST /api/worker/add  (admin panel — manual add) ─────────────────────────

type workerAddRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleWorkerAdd(c *gin.Context) {
	var req workerAddRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "worker_url is required"})
		return
	}

	if err := workerreg.AddAdminWorker(req.WorkerURL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
