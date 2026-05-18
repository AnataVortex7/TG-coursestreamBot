// internal/urlmonitor/model.go
package urlmonitor

import (
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MonitorURL — persisted in MongoDB collection "url_monitors"
type MonitorURL struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"      json:"id"`
	URL             string             `bson:"url"                json:"url"`
	IntervalSeconds int                `bson:"interval_seconds"   json:"interval_seconds"` // min 2
	Enabled         bool               `bson:"enabled"            json:"enabled"`
	CreatedAt       time.Time          `bson:"created_at"         json:"created_at"`
	Label           string             `bson:"label,omitempty"    json:"label,omitempty"` // optional friendly name
}

// PingResult — stored only in memory (last 50 per URL)
type PingResult struct {
	Timestamp  time.Time `json:"timestamp"`
	StatusCode int       `json:"status_code"` // 0 if connection error
	LatencyMs  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	IsUp       bool      `json:"is_up"`
}

// URLStatus — combined live status returned to frontend
type URLStatus struct {
	MonitorURL
	LastResult  *PingResult  `json:"last_result"`  // most recent ping
	History     []PingResult `json:"history"`      // last 50 pings
	IsRunning   bool         `json:"is_running"`   // goroutine active?
	AvgLatency  int64        `json:"avg_latency"`  // avg ms over last 50
	UptimePct   float64      `json:"uptime_pct"`   // % up over last 50
}

// ringBuffer — fixed-size circular buffer for last 50 ping results
type ringBuffer struct {
	mu      sync.Mutex
	results []PingResult
	maxSize int
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		results: make([]PingResult, 0, size),
		maxSize: size,
	}
}

func (r *ringBuffer) Add(p PingResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) >= r.maxSize {
		// shift left — remove oldest
		r.results = r.results[1:]
	}
	r.results = append(r.results, p)
}

func (r *ringBuffer) GetAll() []PingResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PingResult, len(r.results))
	copy(out, r.results)
	return out
}

func (r *ringBuffer) Last() *PingResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return nil
	}
	p := r.results[len(r.results)-1]
	return &p
}

// Stats computes avg latency and uptime % from buffered results
func (r *ringBuffer) Stats() (avgLatency int64, uptimePct float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return 0, 0
	}
	var totalLatency int64
	upCount := 0
	for _, res := range r.results {
		totalLatency += res.LatencyMs
		if res.IsUp {
			upCount++
		}
	}
	avgLatency = totalLatency / int64(len(r.results))
	uptimePct = float64(upCount) / float64(len(r.results)) * 100
	return
}
