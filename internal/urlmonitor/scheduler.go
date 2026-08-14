// internal/urlmonitor/scheduler.go
package urlmonitor

import (
	"net/http"
	"time"
)

// httpClient — shared, with timeout, no redirects followed blindly
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

// workerState — runtime state for one monitored URL
type workerState struct {
	monitorURL MonitorURL
	buffer     *ringBuffer
	stopCh     chan struct{}
	running    bool
}

// newWorkerState — create a fresh worker for a MonitorURL
func newWorkerState(m MonitorURL) *workerState {
	return &workerState{
		monitorURL: m,
		buffer:     newRingBuffer(50),
		stopCh:     make(chan struct{}, 1),
		running:    false,
	}
}

// start — launches the polling goroutine for this worker
func (w *workerState) start() {
	if w.running {
		return
	}
	w.running = true
	go w.loop()
}

// stop — signals the goroutine to exit
func (w *workerState) stop() {
	if !w.running {
		return
	}
	w.running = false
	select {
	case w.stopCh <- struct{}{}:
	default:
	}
}

// loop — ticker-based ping loop, runs until stopCh receives
func (w *workerState) loop() {
	interval := time.Duration(w.monitorURL.IntervalSeconds) * time.Second
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}

	// ping immediately on start
	w.ping()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.ping()
		case <-w.stopCh:
			return
		}
	}
}

// ping — performs one check and stores result in ring buffer
func (w *workerState) ping() {
	result := checkURL(w.monitorURL.URL, w.monitorURL.Token)
	w.buffer.Add(result)
}

// manualPing — trigger one immediate check outside the ticker cycle
func (w *workerState) manualPing() PingResult {
	result := checkURL(w.monitorURL.URL, w.monitorURL.Token)
	w.buffer.Add(result)
	return result
}

// checkURL — dispatches to the right checker based on URL type.
// Hugging Face Space URLs (huggingface.co/spaces/... or *.hf.space) go through
// pingHuggingFace (see hf.go), which asks HF's own Spaces API for the real
// runtime stage — because a Space's public URL is not always available
// directly (private/gated spaces) or can return a misleading 200 OK "loading"
// page while the Space is actually asleep. token — per-monitor HF token,
// vaparla jato private/gated space sathi (rikama asel tar HF_TOKEN env
// var fallback mhanun vaparla jato). Every other URL uses a plain HTTP GET.
func checkURL(rawURL, token string) PingResult {
	if isHuggingFaceURL(rawURL) {
		return pingHuggingFace(rawURL, token)
	}
	return pingHTTP(rawURL)
}

// pingHTTP — plain HTTP GET check (default, non-HF URLs)
func pingHTTP(rawURL string) PingResult {
	start := time.Now()
	result := PingResult{
		Timestamp: start,
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		result.Error = err.Error()
		result.IsUp = false
		result.StatusCode = 0
		return result
	}

	// Free hosting providers (Render, Glitch, etc.) bot / default Go clients la ignore kartat
	// tyamule browser sarakhe headers pathavne garjeche ahe (tarach server wake up hoto).
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := httpClient.Do(req)
	result.LatencyMs = time.Since(start).Milliseconds()

	if err != nil {
		result.Error = err.Error()
		result.IsUp = false
		result.StatusCode = 0
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode
	result.IsUp = resp.StatusCode >= 200 && resp.StatusCode < 400
	return result
}

// status — build URLStatus snapshot for this worker
func (w *workerState) status() URLStatus {
	avgLatency, uptimePct := w.buffer.Stats()
	return URLStatus{
		MonitorURL: w.monitorURL,
		LastResult: w.buffer.Last(),
		History:    w.buffer.GetAll(),
		IsRunning:  w.running,
		AvgLatency: avgLatency,
		UptimePct:  uptimePct,
		HasToken:   w.monitorURL.Token != "",
	}
}
