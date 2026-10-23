package routes

import (
	"mime"
	"net/url"
	"strings"
	"sync"
	"time"
)

// contentDisposition builds a safe Content-Disposition value. The file name comes
// from Telegram (uploader-controlled): quotes / non-ASCII / control characters are
// escaped by mime.FormatMediaType instead of being pasted into the header.
func contentDisposition(kind, filename string) string {
	filename = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, filename)
	if filename == "" {
		return kind
	}
	if v := mime.FormatMediaType(kind, map[string]string{"filename": filename}); v != "" {
		return v
	}
	return kind
}

// isActiveContentMime — types a browser would execute / render as a document
// when served inline from our origin.
func isActiveContentMime(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml",
		"application/xml", "text/javascript", "application/javascript",
		"application/x-javascript", "text/css":
		return true
	}
	return strings.HasSuffix(m, "+xml")
}

// validWorkerURL — http(s) URL with a host and nothing that could break out of the
// CSP header the URL is later concatenated into (spaces, ; , quotes, user:pass@).
func validWorkerURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 300 || strings.ContainsAny(raw, " \t\r\n;,'\"<>") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http"
}

// ── tiny per-key cooldown (spam guard for contact / purchase / notify endpoints) ─

var (
	cooldownMu   sync.Mutex
	cooldownLast = map[string]time.Time{}
)

// allowOnce returns true if key has not been used within d, and records the use.
func allowOnce(key string, d time.Duration) bool {
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	now := time.Now()
	if len(cooldownLast) > 5000 {
		for k, t := range cooldownLast {
			if now.Sub(t) > time.Hour {
				delete(cooldownLast, k)
			}
		}
	}
	if t, ok := cooldownLast[key]; ok && now.Sub(t) < d {
		return false
	}
	cooldownLast[key] = now
	return true
}
