// internal/urlmonitor/scheduler.go
package urlmonitor

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// httpClient — shared, with timeout, no redirects followed blindly
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

const (
	// [NEW] Sleeping service सापडल्यावर "wake burst" mode जास्तीत जास्त
	// एवढा वेळ चालू ठेवतो — यानंतरही live झालं नाही तर सोडून देतो आणि normal
	// (मूळ) ticking interval वर परत जातो — service खरंच बंद/down असू शकतो,
	// कायम burst करत बसणं योग्य नाही.
	wakeMaxDuration = 3 * time.Minute

	// प्रत्येक burst attempt मधलं अंतर — दर इतक्या सेकंदांनी parallel
	// requests चा एक गठ्ठा (batch) पाठवतो.
	wakeBurstInterval = 6 * time.Second

	// प्रत्येक attempt मध्ये किती request **एकाच वेळी, parallel/simultaneous**
	// पाठवायचे — एकच sequential request पाठवून थांबण्यापेक्षा, एकाच वेळी अनेक
	// request गेले की cold-start झालेली service लवकर जागी होते (जास्त
	// concurrent traffic एकाच क्षणी).
	wakeBurstSize = 5
)

// workerState — runtime state for one monitored URL
type workerState struct {
	monitorURL MonitorURL
	buffer     *ringBuffer
	done       chan struct{}
	stopOnce   sync.Once
	running    bool
	waking     int32 // atomic flag — एकाच वेळी एकाच URL साठी एकच wake-burst goroutine चालू राहावा म्हणून
}

// newWorkerState — create a fresh worker for a MonitorURL
func newWorkerState(m MonitorURL) *workerState {
	return &workerState{
		monitorURL: m,
		buffer:     newRingBuffer(50),
		done:       make(chan struct{}),
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

// stop — signals the goroutine (आणि चालू असलेला कुठलाही wake-burst) exit व्हायला.
// close(done) वापरतो (buffered-channel-send ऐवजी) कारण loop() आणि
// wakeBurstLoop() दोन्ही goroutines एकाच वेळी हा signal ऐकत असू शकतात — close
// केल्यावर दोघांनाही एकाच क्षणी कळतं (sync.Once मुळे double-close panic टळतो).
func (w *workerState) stop() {
	if !w.running {
		return
	}
	w.running = false
	w.stopOnce.Do(func() { close(w.done) })
}

// loop — ticker-based ping loop, runs until done चा signal येईपर्यंत
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
		case <-w.done:
			return
		}
	}
}

// ping — performs one check and stores result in ring buffer. Service down/
// sleeping आढळल्यास, background मध्ये wake-burst सुरू करतो (आधीच एक चालू
// असेल तर परत सुरू करत नाही — atomic flag ने guard केलंय).
func (w *workerState) ping() {
	result := checkURL(w.monitorURL.URL, w.monitorURL.Token)
	w.buffer.Add(result)

	if !result.IsUp {
		w.tryStartWakeBurst()
	}
}

// tryStartWakeBurst — फक्त एकच wake-burst goroutine एका वेळी चालू राहील याची
// खात्री करतो (CompareAndSwap — race-safe, दर normal ping वर परत परत नवीन
// burst सुरू होत राहू नये म्हणून).
func (w *workerState) tryStartWakeBurst() {
	if !atomic.CompareAndSwapInt32(&w.waking, 0, 1) {
		return // आधीच एक wake-burst चालू आहे, दुसरा सुरू करायची गरज नाही
	}
	go w.wakeBurstLoop()
}

// wakeBurstLoop — sleeping/down दिसणाऱ्या service ला जागं करण्यासाठी: दर
// wakeBurstInterval ने wakeBurstSize इतके parallel (simultaneous) request
// पाठवतो. यातला कुठलाही request यशस्वी (IsUp=true) झाला की लगेच थांबतो आणि
// normal ticking (loop()) वर नियंत्रण सोडतो. जास्तीत जास्त wakeMaxDuration
// पर्यंतच प्रयत्न करतो — त्यानंतरही live नसेल तर थांबतो (service खरंच बंद
// असू शकतो, कायमचं burst करत राहणं योग्य नाही).
func (w *workerState) wakeBurstLoop() {
	defer atomic.StoreInt32(&w.waking, 0)

	deadline := time.Now().Add(wakeMaxDuration)
	ticker := time.NewTicker(wakeBurstInterval)
	defer ticker.Stop()

	for {
		if time.Now().After(deadline) {
			return // वेळ संपली, service अजून live नाही — प्रयत्न सोडतो, normal ticking चालूच राहील
		}
		if w.burstOnce() {
			return // live झालं — इथेच थांबतो
		}
		select {
		case <-ticker.C:
			continue
		case <-w.done:
			return // monitor स्वतःच बंद होतोय (URL delete / app shutdown) — burst पण थांबव
		}
	}
}

// burstOnce — एकाच क्षणी wakeBurstSize इतके parallel request पाठवतो, सगळे
// पूर्ण होण्याची वाट बघतो. कुठलाही यशस्वी असेल तर तोच result buffer मध्ये save
// करून true देतो; सगळेच fail झाले तर शेवटचा result तरी buffer मध्ये टाकतो
// (dashboard वर latest attempt दिसावा म्हणून) आणि false देतो.
func (w *workerState) burstOnce() bool {
	var wg sync.WaitGroup
	results := make([]PingResult, wakeBurstSize)

	for i := 0; i < wakeBurstSize; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = checkURL(w.monitorURL.URL, w.monitorURL.Token)
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.IsUp {
			w.buffer.Add(r)
			return true
		}
	}
	// सगळेच fail — शेवटचा result तरी नोंदवतो
	w.buffer.Add(results[wakeBurstSize-1])
	return false
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

	resp, err := httpClient.Get(rawURL)
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
