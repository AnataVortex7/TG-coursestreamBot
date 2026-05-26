// Package workerreg manages the pool of external streaming workers.
//
// Workers are deployed separately (e.g. on Render/Koyeb free tier).
// Main server routes stream requests to a healthy worker via round-robin.
// If no workers are healthy, main server falls back to streaming itself.
//
// Workers self-register via POST /api/worker/register on startup and send
// heartbeats every 25s via POST /api/worker/heartbeat.
//
// Admin-added workers (via admin panel):
//   - Main server pings them every 2s to keep Render from suspending them.
//   - If no user is on a course page for 1 hour → ping loop stops (sleep).
//   - When a user opens a course page → WakeAll() resumes the ping loop.
//
// Env-var workers (WORKER_URLS):
//   - Never managed by main server — they are always active on their own.
//
// Configuration (fsb.env):
//
//	WORKER_URLS=https://worker1.koyeb.app,https://worker2.koyeb.app
//	WORKER_HEALTH_INTERVAL_SEC=30
package workerreg

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/presence"
	"go.uber.org/zap"
)

const (
	addedByEnv   = "env"
	addedByAdmin = "admin"

	// adminPingInterval — how often main server pings admin-added workers
	// to keep Render free tier from suspending them.
	adminPingInterval = 2 * time.Second

	// sleepAfter — if no user is active for this long, stop pinging admin workers.
	sleepAfter = 1 * time.Hour

	// sweepInterval — how often presence tracker is swept for stale users.
	sweepInterval = 1 * time.Minute
)

// WorkerNode represents one remote worker instance in the in-memory pool.
type WorkerNode struct {
	BaseURL string
	Healthy bool
	AddedBy string // "env" | "admin"

	// sleeping is true when main server has stopped its 2s keep-alive ping.
	// Only relevant for admin-added workers.
	sleeping bool
}

// Registry holds all registered workers and their health state.
type Registry struct {
	mu      sync.RWMutex
	workers []*WorkerNode
	counter int64 // atomic round-robin index
	log     *zap.Logger

	// adminPingStop — close this channel to stop the admin ping loop.
	// Recreated on WakeAll().
	adminPingStop chan struct{}
	adminPingMu   sync.Mutex
}

var Default = &Registry{}

// Init loads worker URLs from both the DB (active workers) and config env vars,
// merges + deduplicates by URL, runs an initial health check, and starts the
// background loops.
//
// Call this once at startup after config.Load() and db.Connect().
func Init(log *zap.Logger) {
	Default.log = log.Named("WorkerRegistry")

	urlSet := make(map[string]struct{})
	var nodes []*WorkerNode

	// ── 1. DB workers — these were admin-added ────────────────────────────
	dbWorkers, err := db.GetActiveWorkers()
	if err != nil {
		Default.log.Warn("failed to load workers from DB — using env only", zap.Error(err))
	} else {
		for _, w := range dbWorkers {
			u := strings.TrimRight(w.URL, "/")
			if u == "" {
				continue
			}
			addedBy := w.AddedBy
			if addedBy == "" {
				addedBy = addedByAdmin // legacy rows default to admin
			}
			if _, seen := urlSet[u]; !seen {
				urlSet[u] = struct{}{}
				nodes = append(nodes, &WorkerNode{
					BaseURL: u,
					Healthy: false,
					AddedBy: addedBy,
				})
			}
		}
	}

	// ── 2. Env-var workers — merge, mark as "env" ─────────────────────────
	for _, u := range config.ValueOf.WorkerURLs {
		u = strings.TrimRight(u, "/")
		if u == "" {
			continue
		}
		if _, seen := urlSet[u]; !seen {
			urlSet[u] = struct{}{}
			nodes = append(nodes, &WorkerNode{
				BaseURL: u,
				Healthy: false,
				AddedBy: addedByEnv,
			})
		} else {
			// URL already in DB — keep AddedBy from DB but note it is also in env
			// (treated as env = never sleep)
			Default.mu.Lock()
			for _, n := range nodes {
				if n.BaseURL == u {
					n.AddedBy = addedByEnv
				}
			}
			Default.mu.Unlock()
		}
	}

	if len(nodes) == 0 {
		Default.log.Info("No worker URLs configured — using main server for streaming")
		return
	}

	Default.mu.Lock()
	Default.workers = nodes
	Default.mu.Unlock()

	// ── 3. Initial health check (blocking) ───────────────────────────────
	Default.checkAll()
	Default.log.Sugar().Infof("Worker pool ready: %d registered, %d healthy",
		Default.total(), Default.HealthyCount())

	// ── 4. Background loops ───────────────────────────────────────────────
	go Default.healthLoop()       // health check for all workers
	go Default.sleepWatchLoop()   // decides when to sleep/wake admin workers
	Default.startAdminPingLoop()  // 2s keep-alive ping for admin workers
}

// ─── Public API ───────────────────────────────────────────────────────────────

// Next returns the next healthy worker URL using priority round-robin:
//  1. Env-var workers (always active, highest priority)
//  2. Admin-added awake workers
//  3. Returns "" → caller falls back to local stream
func Next() string {
	Default.mu.RLock()
	defer Default.mu.RUnlock()

	// Priority 1: env workers
	envHealthy := make([]*WorkerNode, 0)
	for _, w := range Default.workers {
		if w.Healthy && w.AddedBy == addedByEnv {
			envHealthy = append(envHealthy, w)
		}
	}
	if len(envHealthy) > 0 {
		idx := atomic.AddInt64(&Default.counter, 1)
		return envHealthy[int(idx)%len(envHealthy)].BaseURL
	}

	// Priority 2: admin workers that are awake + healthy
	adminHealthy := make([]*WorkerNode, 0)
	for _, w := range Default.workers {
		if w.Healthy && w.AddedBy == addedByAdmin && !w.sleeping {
			adminHealthy = append(adminHealthy, w)
		}
	}
	if len(adminHealthy) > 0 {
		idx := atomic.AddInt64(&Default.counter, 1)
		return adminHealthy[int(idx)%len(adminHealthy)].BaseURL
	}

	return ""
}

// RegisterWorker is called by POST /api/worker/register (worker self-registers on startup).
// Marks AddedBy="admin" — worker deployed on Render registers itself.
func RegisterWorker(url string) {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return
	}

	if err := db.UpsertWorker(url, addedByAdmin); err != nil {
		Default.log.Warn("RegisterWorker: DB upsert failed", zap.String("url", url), zap.Error(err))
	}

	Default.mu.Lock()
	found := false
	for _, w := range Default.workers {
		if w.BaseURL == url {
			found = true
			break
		}
	}
	if !found {
		Default.workers = append(Default.workers, &WorkerNode{
			BaseURL: url,
			Healthy: false,
			AddedBy: addedByAdmin,
		})
	}
	Default.mu.Unlock()

	go func() {
		healthy := Default.ping(url)
		Default.mu.Lock()
		for _, w := range Default.workers {
			if w.BaseURL == url {
				w.Healthy = healthy
				w.sleeping = false // just registered = awake
				break
			}
		}
		Default.mu.Unlock()

		status := "active"
		if !healthy {
			status = "unhealthy"
		}
		if err := db.UpdateWorkerStatus(url, status); err != nil {
			Default.log.Warn("RegisterWorker: DB status update failed", zap.String("url", url), zap.Error(err))
		}
		Default.log.Sugar().Infof("RegisterWorker: %s healthy=%v", url, healthy)
	}()
}

// AddAdminWorker adds a worker URL via admin panel.
// Same as RegisterWorker but explicitly marked AddedBy="admin".
func AddAdminWorker(url string) error {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return nil
	}

	if err := db.UpsertWorker(url, addedByAdmin); err != nil {
		return err
	}

	Default.mu.Lock()
	found := false
	for _, w := range Default.workers {
		if w.BaseURL == url {
			found = true
			break
		}
	}
	if !found {
		Default.workers = append(Default.workers, &WorkerNode{
			BaseURL: url,
			Healthy: false,
			AddedBy: addedByAdmin,
			sleeping: false,
		})
	}
	Default.mu.Unlock()

	// Immediate health check
	go func() {
		healthy := Default.ping(url)
		Default.mu.Lock()
		for _, w := range Default.workers {
			if w.BaseURL == url {
				w.Healthy = healthy
				break
			}
		}
		Default.mu.Unlock()
		status := "active"
		if !healthy {
			status = "unhealthy"
		}
		_ = db.UpdateWorkerStatus(url, status)
		Default.log.Sugar().Infof("AddAdminWorker: %s healthy=%v", url, healthy)
	}()

	return nil
}

// HeartbeatWorker is called by POST /api/worker/heartbeat.
// Refreshes last_seen in DB and marks the worker active in-memory.
func HeartbeatWorker(url string) {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return
	}

	if err := db.UpdateWorkerLastSeen(url); err != nil {
		Default.log.Warn("HeartbeatWorker: DB update failed", zap.String("url", url), zap.Error(err))
	}

	Default.mu.Lock()
	found := false
	for _, w := range Default.workers {
		if w.BaseURL == url {
			w.Healthy = true
			w.sleeping = false
			found = true
			break
		}
	}
	Default.mu.Unlock()

	if !found {
		Default.mu.Lock()
		Default.workers = append(Default.workers, &WorkerNode{
			BaseURL: url,
			Healthy: true,
			AddedBy: addedByAdmin,
		})
		Default.mu.Unlock()
		Default.log.Info("HeartbeatWorker: new worker added to pool", zap.String("url", url))
	}
}

// RemoveWorker removes a worker from both DB and in-memory pool.
func RemoveWorker(url string) error {
	url = strings.TrimRight(url, "/")

	if err := db.DeleteWorker(url); err != nil {
		return err
	}

	Default.mu.Lock()
	filtered := Default.workers[:0]
	for _, w := range Default.workers {
		if w.BaseURL != url {
			filtered = append(filtered, w)
		}
	}
	Default.workers = filtered
	Default.mu.Unlock()

	Default.log.Info("RemoveWorker: removed", zap.String("url", url))
	return nil
}

// WakeAll wakes all sleeping admin-added workers by sending a /ping to each
// and restarting the 2s keep-alive loop.
// Called when a user opens a course page.
func WakeAll() {
	Default.mu.Lock()
	for _, w := range Default.workers {
		if w.AddedBy == addedByAdmin && w.sleeping {
			w.sleeping = false
		}
	}
	workers := make([]*WorkerNode, len(Default.workers))
	copy(workers, Default.workers)
	Default.mu.Unlock()

	// Fire a /ping at each admin worker to wake Render
	for _, w := range workers {
		if w.AddedBy == addedByAdmin {
			go func(url string) {
				_ = Default.ping(url) // result ignored — just waking Render
			}(w.BaseURL)
		}
	}

	// Restart 2s ping loop
	Default.startAdminPingLoop()
	Default.log.Info("WakeAll: woke admin workers")
}

// HealthyCount returns the number of currently healthy workers.
func HealthyCount() int {
	return Default.HealthyCount()
}

func (r *Registry) HealthyCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, w := range r.workers {
		if w.Healthy {
			count++
		}
	}
	return count
}

func (r *Registry) total() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.workers)
}

// Status returns a snapshot of all workers for admin endpoints.
func Status() []map[string]interface{} {
	Default.mu.RLock()
	defer Default.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(Default.workers))
	for _, w := range Default.workers {
		out = append(out, map[string]interface{}{
			"url":      w.BaseURL,
			"healthy":  w.Healthy,
			"added_by": w.AddedBy,
			"sleeping": w.sleeping,
		})
	}
	return out
}

// ─── Internal loops ───────────────────────────────────────────────────────────

// healthLoop runs periodic health checks for all workers.
func (r *Registry) healthLoop() {
	interval := time.Duration(config.ValueOf.WorkerHealthIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		r.checkAll()
	}
}

// sleepWatchLoop checks presence every minute and puts admin workers to sleep
// if no user has been active for sleepAfter (1 hour).
func (r *Registry) sleepWatchLoop() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		// Sweep stale presence entries
		presence.Sweep()

		if presence.IsAnyoneActive() {
			continue
		}
		// No one active — check how long since last activity
		if time.Since(presence.LastActiveTime()) < sleepAfter {
			continue
		}
		// 1 hour passed with no activity → sleep admin workers
		r.sleepAdminWorkers()
	}
}

// sleepAdminWorkers marks admin workers as sleeping and stops the ping loop.
func (r *Registry) sleepAdminWorkers() {
	r.mu.Lock()
	anySleeping := false
	for _, w := range r.workers {
		if w.AddedBy == addedByAdmin && !w.sleeping {
			w.sleeping = true
			anySleeping = true
		}
	}
	r.mu.Unlock()

	if anySleeping {
		r.stopAdminPingLoop()
		r.log.Info("sleepAdminWorkers: admin workers are now sleeping (no active users for 1h)")
	}
}

// startAdminPingLoop starts a goroutine that pings admin workers every 2s.
// If a loop is already running, it is stopped first.
func (r *Registry) startAdminPingLoop() {
	r.adminPingMu.Lock()
	defer r.adminPingMu.Unlock()

	// Stop existing loop if running
	if r.adminPingStop != nil {
		close(r.adminPingStop)
	}
	stop := make(chan struct{})
	r.adminPingStop = stop

	go func() {
		ticker := time.NewTicker(adminPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.RLock()
				var adminURLs []string
				for _, w := range r.workers {
					if w.AddedBy == addedByAdmin && !w.sleeping {
						adminURLs = append(adminURLs, w.BaseURL)
					}
				}
				r.mu.RUnlock()

				for _, url := range adminURLs {
					go func(u string) {
						healthy := r.ping(u)
						r.mu.Lock()
						for _, w := range r.workers {
							if w.BaseURL == u {
								w.Healthy = healthy
								break
							}
						}
						r.mu.Unlock()
					}(url)
				}
			}
		}
	}()
}

// stopAdminPingLoop stops the 2s admin ping goroutine.
func (r *Registry) stopAdminPingLoop() {
	r.adminPingMu.Lock()
	defer r.adminPingMu.Unlock()
	if r.adminPingStop != nil {
		close(r.adminPingStop)
		r.adminPingStop = nil
	}
}

func (r *Registry) checkAll() {
	r.mu.RLock()
	workers := make([]*WorkerNode, len(r.workers))
	copy(workers, r.workers)
	r.mu.RUnlock()

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(node *WorkerNode) {
			defer wg.Done()
			healthy := r.ping(node.BaseURL)

			r.mu.Lock()
			node.Healthy = healthy
			r.mu.Unlock()

			status := "active"
			if !healthy {
				status = "unhealthy"
				r.log.Warn("worker unhealthy", zap.String("url", node.BaseURL))
			}
			if err := db.UpdateWorkerStatus(node.BaseURL, status); err != nil {
				r.log.Warn("health check: DB status update failed",
					zap.String("url", node.BaseURL), zap.Error(err))
			}
		}(w)
	}
	wg.Wait()
}

type pingResponse struct {
	OK bool `json:"ok"`
}

func (r *Registry) ping(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ping", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}
	var pr pingResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return false
	}
	return pr.OK
}
