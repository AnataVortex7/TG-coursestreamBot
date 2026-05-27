package routes

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"EverythingSuckz/fsb/internal/db"
	workerreg "EverythingSuckz/fsb/internal/worker"

	"github.com/gin-gonic/gin"
)

// LoadWorkerAPI registers all worker-related routes.
//
// Public (worker-to-server, protected by X-Worker-Secret):
//   POST /api/worker/register   — worker self-registers on startup
//   POST /api/worker/heartbeat  — worker heartbeat every 25s
//
// Admin-only (moved under /api/admin/worker/* via LoadAdminAPI):
//   GET    /api/admin/worker/list
//   POST   /api/admin/worker/add
//   DELETE /api/admin/worker/remove
//
// Note: Admin endpoints are registered in LoadAdminAPI (admin_api.go) so they
// automatically inherit adminAuthMiddleware() — admin session + CSRF + Origin.

func (e *allRoutes) LoadWorkerAPI(r *Route) {
	// ── Worker self-registration (no user auth — protected by STREAM_SECRET) ──
	r.Engine.POST("/api/worker/register", handleWorkerRegister)
	r.Engine.POST("/api/worker/heartbeat", handleWorkerHeartbeat)

	// ── Stream key check — worker polls this every 5s to verify its stream
	// is still the latest active one for the user. ──────────────────────────
	r.Engine.GET("/api/worker/stream-check", handleWorkerStreamCheck)
}

// ── POST /api/worker/register ─────────────────────────────────────────────────
// Called by the worker process on startup to register itself with main server.
// Protected by X-Worker-Secret header (STREAM_SECRET env var).

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
// Called by the worker every 25s to signal it is still alive.
// Protected by X-Worker-Secret header.

type workerHeartbeatRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
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

// ── GET /api/worker/stream-check ──────────────────────────────────────────────
// Worker दर 5 सेकंदाला हे call करतो हे verify करण्यासाठी की त्याचा stream
// अजूनही latest active stream आहे का.
//
// Auth:   X-Worker-Secret header (= STREAM_SECRET env var)
// Params: uid (userIDHex), key (streamKey issued with the worker token)
//
// Response:
//   {"active": true}  — key match, stream valid, चालू ठेव
//   {"active": false} — key mismatch (user started new stream), stream बंद कर
//
// Worker ला 2 consecutive non-active/fail responses आल्यास stream cancel करायचा.

func handleWorkerStreamCheck(c *gin.Context) {
	// 1. Auth — same STREAM_SECRET used by register/heartbeat
	secret := c.GetHeader("X-Worker-Secret")
	expected := os.Getenv("STREAM_SECRET")
	if subtle.ConstantTimeCompare([]byte(secret), []byte(expected)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return
	}

	// 2. Params
	uid := strings.TrimSpace(c.Query("uid"))
	key := strings.TrimSpace(c.Query("key"))
	if uid == "" || key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "uid and key are required"})
		return
	}

	// 3. Check against in-memory registry
	val, exists := activeStreamKeys.Load(uid)
	if !exists {
		// No active stream registered for this user at all
		c.JSON(http.StatusOK, gin.H{"active": false})
		return
	}

	active := val.(string) == key
	c.JSON(http.StatusOK, gin.H{"active": active})
}

// ── Admin worker handlers (registered in LoadAdminAPI under adminAuthMiddleware) ──

// GET /api/admin/worker/list
func handleAdminWorkerList(c *gin.Context) {
	workers, err := db.GetAllWorkers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}

	// Enrich with live in-memory status
	liveMap := make(map[string]map[string]interface{})
	for _, s := range workerreg.Status() {
		url, _ := s["url"].(string)
		liveMap[url] = s
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
		healthy, _ := live["healthy"].(bool)
		sleeping, _ := live["sleeping"].(bool)
		addedBy := w.AddedBy
		if addedBy == "" {
			if ab, ok := live["added_by"].(string); ok {
				addedBy = ab
			}
		}
		out = append(out, workerView{
			URL:      w.URL,
			Status:   w.Status,
			AddedBy:  addedBy,
			Healthy:  healthy,
			Sleeping: sleeping,
			LastSeen: w.LastSeen.Format("2006-01-02 15:04:05 UTC"),
			AddedAt:  w.AddedAt.Format("2006-01-02 15:04:05 UTC"),
		})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "workers": out})
}

// POST /api/admin/worker/add
type workerAddRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleAdminWorkerAdd(c *gin.Context) {
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

// DELETE /api/admin/worker/remove
type workerRemoveRequest struct {
	WorkerURL string `json:"worker_url" binding:"required"`
}

func handleAdminWorkerRemove(c *gin.Context) {
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
