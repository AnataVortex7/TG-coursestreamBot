// Package workerreg manages the pool of external streaming workers.
//
// Workers are deployed separately (e.g. on Koyeb free tier).
// Main server routes stream requests to a healthy worker via round-robin.
// If no workers are healthy, main server falls back to streaming itself.
//
// Workers self-register via POST /api/worker/register on startup and send
// heartbeats every 25s via POST /api/worker/heartbeat.
//
// Configuration (fsb.env):
//   WORKER_URLS=https://worker1.koyeb.app,https://worker2.koyeb.app
//   WORKER_HEALTH_INTERVAL_SEC=30

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
	"go.uber.org/zap"
)

// WorkerNode represents one remote worker instance in the in-memory pool.
type WorkerNode struct {
	BaseURL string
	Healthy bool
}

// Registry holds all registered workers and their health state.
type Registry struct {
	mu      sync.RWMutex
	workers []*WorkerNode
	counter int64 // atomic round-robin index
	log     *zap.Logger
}

var Default = &Registry{}

// Init loads worker URLs from both the DB (active workers) and config env vars,
// merges + deduplicates by URL, runs an initial health check, and starts the
// background health-check loop.
//
// Call this once at startup after config.Load() and db.Connect().
func Init(log *zap.Logger) {
	Default.log = log.Named("WorkerRegistry")

	// ── 1. Collect URLs from DB ────────────────────────────────────────────
	urlSet := make(map[string]struct{})
	var mergedURLs []string

	dbWorkers, err := db.GetActiveWorkers()
	if err != nil {
		Default.log.Warn("failed to load workers from DB — using env only", zap.Error(err))
	} else {
		for _, w := range dbWorkers {
			u := strings.TrimRight(w.URL, "/")
			if _, seen := urlSet[u]; !seen {
				urlSet[u] = struct{}{}
				mergedURLs = append(mergedURLs, u)
			}
		}
	}

	// ── 2. Merge env URLs (deduplicated) ──────────────────────────────────
	for _, u := range config.ValueOf.WorkerURLs {
		u = strings.TrimRight(u, "/")
		if u == "" {
			continue
		}
		if _, seen := urlSet[u]; !seen {
			urlSet[u] = struct{}{}
			mergedURLs = append(mergedURLs, u)
		}
	}

	if len(mergedURLs) == 0 {
		Default.log.Info("No worker URLs configured — using main server for streaming")
		return
	}

	// ── 3. Build in-memory pool ───────────────────────────────────────────
	Default.mu.Lock()
	for _, u := range mergedURLs {
		Default.workers = append(Default.workers, &WorkerNode{
			BaseURL: u,
			Healthy: false, // set by first health check below
		})
	}
	Default.mu.Unlock()

	// ── 4. Initial health check (blocking) + update DB status ─────────────
	Default.checkAll()
	Default.log.Sugar().Infof("Worker pool ready: %d registered, %d healthy",
		Default.total(), Default.HealthyCount())

	// ── 5. Background health-check loop ───────────────────────────────────
	go Default.healthLoop()
}

// Next returns the base URL of the next healthy worker (round-robin).
// Returns "" if no healthy workers are available → caller should use local stream.
func Next() string {
	Default.mu.RLock()
	defer Default.mu.RUnlock()

	healthy := make([]*WorkerNode, 0, len(Default.workers))
	for _, w := range Default.workers {
		if w.Healthy {
			healthy = append(healthy, w)
		}
	}
	if len(healthy) == 0 {
		return ""
	}
	idx := atomic.AddInt64(&Default.counter, 1)
	return healthy[int(idx)%len(healthy)].BaseURL
}

// RegisterWorker is called by POST /api/worker/register.
// It upserts the worker in DB, adds it to the in-memory pool (if new),
// and runs an immediate health check.
func RegisterWorker(url string) {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return
	}

	// Upsert in DB
	if err := db.UpsertWorker(url); err != nil {
		Default.log.Warn("RegisterWorker: DB upsert failed", zap.String("url", url), zap.Error(err))
	}

	// Add to in-memory pool if not already present
	Default.mu.Lock()
	found := false
	for _, w := range Default.workers {
		if w.BaseURL == url {
			found = true
			break
		}
	}
	if !found {
		Default.workers = append(Default.workers, &WorkerNode{BaseURL: url, Healthy: false})
	}
	Default.mu.Unlock()

	// Immediate health check for this worker
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
		if err := db.UpdateWorkerStatus(url, status); err != nil {
			Default.log.Warn("RegisterWorker: DB status update failed", zap.String("url", url), zap.Error(err))
		}
		Default.log.Sugar().Infof("RegisterWorker: %s healthy=%v", url, healthy)
	}()
}

// HeartbeatWorker is called by POST /api/worker/heartbeat.
// It refreshes last_seen in DB and marks the worker active in-memory.
func HeartbeatWorker(url string) {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return
	}

	// Refresh last_seen + status in DB
	if err := db.UpdateWorkerLastSeen(url); err != nil {
		Default.log.Warn("HeartbeatWorker: DB update failed", zap.String("url", url), zap.Error(err))
	}

	// Mark healthy in memory (heartbeat = worker is alive)
	Default.mu.Lock()
	found := false
	for _, w := range Default.workers {
		if w.BaseURL == url {
			w.Healthy = true
			found = true
			break
		}
	}
	Default.mu.Unlock()

	// Worker not yet in memory pool (race: registered after Init) — add it
	if !found {
		Default.mu.Lock()
		Default.workers = append(Default.workers, &WorkerNode{BaseURL: url, Healthy: true})
		Default.mu.Unlock()
		Default.log.Info("HeartbeatWorker: new worker added to pool", zap.String("url", url))
	}
}

// RemoveWorker removes a worker from both DB and in-memory pool.
// Called by DELETE /api/worker/remove.
func RemoveWorker(url string) error {
	url = strings.TrimRight(url, "/")

	// Remove from DB
	if err := db.DeleteWorker(url); err != nil {
		return err
	}

	// Remove from in-memory pool
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

// Status returns a snapshot of all workers and their health for /ping or admin endpoints.
func Status() []map[string]interface{} {
	Default.mu.RLock()
	defer Default.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(Default.workers))
	for _, w := range Default.workers {
		out = append(out, map[string]interface{}{
			"url":     w.BaseURL,
			"healthy": w.Healthy,
		})
	}
	return out
}

// ─── Internal ─────────────────────────────────────────────────────────────────

func (r *Registry) healthLoop() {
	interval := time.Duration(config.ValueOf.WorkerHealthIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		r.checkAll()
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

			// Sync status back to DB
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
