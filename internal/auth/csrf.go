// internal/auth/csrf.go
// CSRF protection — double-submit cookie pattern.
//
// Flow:
//  1. GET /admin (किंवा कोणतेही page load) — EnsureCSRFCookie middleware
//     fsb_csrf cookie set करतो (non-HttpOnly, SameSite=Strict).
//  2. JS getCsrfToken() cookie वाचतो आणि X-CSRF-Token header म्हणून
//     सर्व POST/DELETE requests मध्ये पाठवतो.
//  3. CSRFRequired middleware: header value === cookie value validate करतो.
//     Match नाही → 403 Forbidden.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	csrfCookieName = "fsb_csrf"
	CSRFHeader     = "X-CSRF-Token"
	csrfMaxAge     = 86400 // 24 hours
)

// generateCSRFToken — cryptographically secure 32-byte hex token.
func generateCSRFToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// EnsureCSRFCookie — GET handler मध्ये middleware म्हणून वापरा.
// fsb_csrf cookie नसल्यास नवीन random token set करतो.
//
// SameSite=Strict: cross-site requests cookie पाठवत नाहीत.
// HttpOnly=false: JS ला cookie readable असणे आवश्यक (double-submit pattern).
func EnsureCSRFCookie(c *gin.Context) {
	if _, err := c.Cookie(csrfCookieName); err != nil {
		tok := generateCSRFToken()
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     csrfCookieName,
			Value:    tok,
			Path:     "/",
			MaxAge:   csrfMaxAge,
			SameSite: http.SameSiteStrictMode,
			HttpOnly: false,
		})
	}
	c.Next()
}

// CSRFRequired — POST / DELETE / PUT / PATCH requests वर X-CSRF-Token header validate करतो.
// GET आणि HEAD requests skip केले जातात.
//
// Validation: header value bcrypt-compare नाही, constant-time string compare वापरतो
// (timing attack prevent करायला).
func CSRFRequired(c *gin.Context) {
	method := c.Request.Method
	if method == http.MethodGet ||
		method == http.MethodHead ||
		method == http.MethodOptions {
		c.Next()
		return
	}

	cookieToken, err := c.Cookie(csrfCookieName)
	if err != nil || cookieToken == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "CSRF token missing. Please refresh the page.",
		})
		return
	}

	headerToken := strings.TrimSpace(c.GetHeader(CSRFHeader))
	if headerToken == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "X-CSRF-Token header required.",
		})
		return
	}

	if !csrfSecureCompare(headerToken, cookieToken) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "Invalid CSRF token.",
		})
		return
	}

	c.Next()
}

// csrfSecureCompare — constant-time string comparison.
// Standard == operator timing attack ला vulnerable असतो.
func csrfSecureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
