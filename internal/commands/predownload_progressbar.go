// internal/commands/predownload_progressbar.go
//
// [NEW] /predownload साठी वेगळा, स्वतंत्र "visual progress bar" ticker.
// हा existing per-completion editProgress ला touch करत नाही — तो जसा आहे
// तसाच चालू राहतो. हा फक्त दर 5 सेकंदांनी संपूर्ण batch चा एकत्रित overview
// (bar + counts) एका वेगळ्या ticker मधून पाठवतो, जेणेकरून admin ला संपूर्ण
// job ची स्थिती (किती % झालं, किती processing/done/skip/err) साध्या नजरेत
// दिसेल — मध्ये कितीही jobs पूर्ण/अपूर्ण असोत.
package commands

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	jobSubmittedListeners sync.Map // key: uint64, value: func(messageID int, jobID string)
	listenerSeq           uint64

	// OnJobSubmitted — optional hook kept for backwards compatibility.
	OnJobSubmitted func(messageID int, jobID string)
)

// AddJobSubmittedHook registers a listener function for job submission events.
// It returns an unregister function that cleans up the listener when done.
func AddJobSubmittedHook(fn func(messageID int, jobID string)) func() {
	id := atomic.AddUint64(&listenerSeq, 1)
	jobSubmittedListeners.Store(id, fn)
	return func() {
		jobSubmittedListeners.Delete(id)
	}
}

// NotifyJobSubmitted triggers all registered job submission hooks.
func NotifyJobSubmitted(messageID int, jobID string) {
	if OnJobSubmitted != nil {
		OnJobSubmitted(messageID, jobID)
	}
	jobSubmittedListeners.Range(func(_, value any) bool {
		if fn, ok := value.(func(int, string)); ok && fn != nil {
			fn(messageID, jobID)
		}
		return true
	})
}

// RenderProgressBar — 20-block textual progress bar तयार करतो.
// उदा: [██████████░░░░░░░░░░] 50%
func RenderProgressBar(completed, total int) string {
	const width = 20
	if total <= 0 {
		return "[" + strings.Repeat("░", width) + "] 0%"
	}
	pct := float64(completed) / float64(total)
	if pct > 1 {
		pct = 1
	} else if pct < 0 {
		pct = 0
	}
	filled := int(pct*float64(width) + 0.5) // rounded
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s] %d%%", bar, int(pct*100+0.5))
}

// StartProgressBarTicker — दर 5 सेकंदांनी snapshot() मधून सध्याची स्थिती
// (done, skipped, failed, processing, total) वाचून visual progress-bar सहित
// text तयार करतो आणि send() ला देतो (उदा. existing editProgress function).
// detailLines() (nil चालेल) सध्या in-flight असलेल्या प्रत्येक lecture चा
// stage/percent दाखवणाऱ्या ओळी परत करतो (उदा. "• Lecture 41 — 📥 Downloading 63%").
//
// stopCh बंद (closed) केला की ticker आपोआप थांबतो — त्यामुळे batch संपल्यावर
// defer/close ने नीट cleanup होतो, कुठलाही goroutine leak राहत नाही.
func StartProgressBarTicker(
	stopCh <-chan struct{},
	label string,
	snapshot func() (done, skipped, failed, processing, total int),
	detailLines func() []string,
	send func(text string),
) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				done, skipped, failed, processing, total := snapshot()
				completed := done + skipped + failed
				text := fmt.Sprintf(
					"%s\n\n%s\n📦 %d/%d lectures\n🔄 Processing: %d | ✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d",
					label, RenderProgressBar(completed, total), completed, total, processing, done, skipped, failed,
				)
				if detailLines != nil {
					if lines := detailLines(); len(lines) > 0 {
						text += "\n\n" + strings.Join(lines, "\n")
					}
				}
				text += "\n\n🛑 Stop: /stopuserbotdl"
				send(text)
			case <-stopCh:
				return
			}
		}
	}()
}
