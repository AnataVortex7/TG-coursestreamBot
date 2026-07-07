// internal/urlmonitor/manager.go
package urlmonitor

import (
	"sync"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// Manager — singleton that owns all workerStates
type Manager struct {
	mu      sync.RWMutex
	workers map[primitive.ObjectID]*workerState
	log     *zap.Logger
}

var globalManager *Manager

// Init — called once from run.go after db.Connect()
// Loads all saved URLs from MongoDB and starts their goroutines
func Init(log *zap.Logger) {
	log = log.Named("urlmonitor")
	globalManager = &Manager{
		workers: make(map[primitive.ObjectID]*workerState),
		log:     log,
	}

	urls, err := ListURLs()
	if err != nil {
		log.Error("Failed to load monitored URLs from DB", zap.Error(err))
		return
	}

	for _, u := range urls {
		if u.Enabled {
			globalManager.startWorker(u)
		}
	}

	log.Sugar().Infof("URL Monitor started — %d URL(s) loaded", len(urls))
}

// startWorker — internal: create + start a worker (must hold no lock, or call with mu held)
func (m *Manager) startWorker(u MonitorURL) {
	w := newWorkerState(u)
	w.start()
	m.workers[u.ID] = w
	m.log.Sugar().Infof("Monitoring started: %s (every %ds)", u.URL, u.IntervalSeconds)
}

// AddURL — save to Mongo + start goroutine
func AddURL(url, label string, intervalSeconds int, token string) (*MonitorURL, error) {
	doc, err := SaveURL(url, label, intervalSeconds, token)
	if err != nil {
		return nil, err
	}
	globalManager.mu.Lock()
	globalManager.startWorker(*doc)
	globalManager.mu.Unlock()
	return doc, nil
}

// RemoveURL — stop goroutine + delete from Mongo
func RemoveURL(id primitive.ObjectID) error {
	globalManager.mu.Lock()
	if w, ok := globalManager.workers[id]; ok {
		w.stop()
		delete(globalManager.workers, id)
	}
	globalManager.mu.Unlock()
	return DeleteURL(id)
}

// GetStatus — return URLStatus for one URL
func GetStatus(id primitive.ObjectID) (*URLStatus, error) {
	globalManager.mu.RLock()
	w, ok := globalManager.workers[id]
	globalManager.mu.RUnlock()

	if !ok {
		// not in memory — try DB (disabled URL)
		doc, err := GetURLByID(id)
		if err != nil {
			return nil, err
		}
		s := URLStatus{MonitorURL: *doc, IsRunning: false}
		return &s, nil
	}

	s := w.status()
	return &s, nil
}

// ListAllStatus — return URLStatus for every monitored URL
func ListAllStatus() []URLStatus {
	globalManager.mu.RLock()
	defer globalManager.mu.RUnlock()

	out := make([]URLStatus, 0, len(globalManager.workers))
	for _, w := range globalManager.workers {
		out = append(out, w.status())
	}
	return out
}

// ManualCheck — trigger one immediate ping for a URL
func ManualCheck(id primitive.ObjectID) (*PingResult, error) {
	globalManager.mu.RLock()
	w, ok := globalManager.workers[id]
	globalManager.mu.RUnlock()

	if !ok {
		doc, err := GetURLByID(id)
		if err != nil {
			return nil, err
		}
		// start a temp worker just for this ping
		tmp := newWorkerState(*doc)
		result := tmp.manualPing()
		return &result, nil
	}

	result := w.manualPing()
	return &result, nil
}

// GetHistory — return last 50 ping results for a URL
func GetHistory(id primitive.ObjectID) ([]PingResult, error) {
	globalManager.mu.RLock()
	w, ok := globalManager.workers[id]
	globalManager.mu.RUnlock()

	if !ok {
		return []PingResult{}, nil
	}
	return w.buffer.GetAll(), nil
}

// StopAll — gracefully stop all workers (called on app shutdown)
func StopAll() {
	globalManager.mu.Lock()
	defer globalManager.mu.Unlock()
	for _, w := range globalManager.workers {
		w.stop()
	}
	globalManager.log.Info("URL Monitor: all workers stopped")
}
