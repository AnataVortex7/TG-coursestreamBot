// internal/auth/otp_auth.go
// OTP-based email login handlers + password fallback.
// These sit alongside the existing auth.go handlers in the same package.
package auth

import (
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"fmt"
	"log"
	"crypto/rand"
    "math/big"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const maxOTPAttempts = 3

// ─── Service health ───────────────────────────────────────────────────────────

// CheckEmailService — GET /login/check-service
// Returns JSON {"up": true/false} so the frontend can decide which login mode to show.
func CheckEmailService(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"up": email.IsServiceUp()})
}

// ─── Send OTP ─────────────────────────────────────────────────────────────────

// SendOTPHandler — POST /login/otp/send
// Body (form): email=user@example.com
//
// Security notes:
//   - Returns the same success message whether email exists or not (prevents user enumeration).
//   - Rate-limited: 1 OTP per 60 seconds per email address.
//   - OTP is stored as a bcrypt hash — never plain text.
func SendOTPHandler(c *gin.Context) {
	emailAddr := strings.ToLower(strings.TrimSpace(c.PostForm("email")))

	if emailAddr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email address is required."})
		return
	}

	// Verify user exists and is active (but don't reveal which is the case to caller)
	user, err := db.GetUserByMobileOrEmail(emailAddr)
	if err != nil || user == nil {
		// Generic response — don't reveal non-existence
		c.JSON(http.StatusOK, gin.H{"message": "OTP sent! Check your inbox."})
		return
	}
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "Your account is disabled. Contact admin."})
		return
	}

	// Rate limit: 60 seconds between OTPs
	if !db.CanSendOTP(emailAddr) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Please wait 60 seconds before requesting a new OTP.",
		})
		return
	}

	// Generate 4-digit OTP
	otpPlain := generateOTP()

	// Hash before storing (bcrypt — same helper as passwords)
	otpHash, err := HashPassword(otpPlain)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	if err := db.CreateOTP(emailAddr, otpHash); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	// Send via Brevo SMTP
	if err := email.SendOTP(emailAddr, otpPlain); err != nil {
		// Log the real error so it appears in Koyeb / Docker logs.
		// Check: Koyeb dashboard → your app → Logs tab
		log.Printf("[OTP] SendOTP FAILED for %s: %v", emailAddr, err)

		// Delete the OTP record so the 60-second rate limit does NOT block
		// the user from retrying immediately after a failed send.
		_ = db.DeleteOTP(emailAddr)

		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Failed to send OTP email. Please try again or use password login.",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "OTP sent! Check your inbox."})
}

// ─── Verify OTP ───────────────────────────────────────────────────────────────

// VerifyOTPHandler — POST /login/otp/verify
// Body (form): email=..., otp=1234
//
// On success: sets the session cookie and returns {"redirect": "/courses"}.
// On failure: returns {"error": "..."} with appropriate HTTP status.
func VerifyOTPHandler(c *gin.Context) {
	emailAddr := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	otpCode   := strings.TrimSpace(c.PostForm("otp"))

	if emailAddr == "" || otpCode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email and OTP are required."})
		return
	}
	if len(otpCode) != 4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTP must be exactly 4 digits."})
		return
	}

	// Fetch active OTP record
	rec, err := db.GetValidOTP(emailAddr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}
	if rec == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTP expired or not found. Request a new one."})
		return
	}

	// Too many wrong attempts?
	if rec.Attempts >= maxOTPAttempts {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Too many wrong attempts. Please request a new OTP.",
		})
		return
	}

	// Verify OTP (bcrypt compare)
	if !CheckPassword(otpCode, rec.OTPHash) {
		_ = db.IncrementOTPAttempts(rec.ID)
		remaining := maxOTPAttempts - rec.Attempts - 1
		if remaining <= 0 {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Wrong OTP. No attempts left. Request a new OTP.",
			})
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": fmt.Sprintf("Wrong OTP. %d attempt(s) remaining.", remaining),
			})
		}
		return
	}

	// ✓ OTP correct — consume it
	_ = db.MarkOTPUsed(rec.ID)

	// Fetch the user record to issue a session
	user, err := db.GetUserByMobileOrEmail(emailAddr)
	if err != nil || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	// Issue JWT
	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	ip := clientIP(c)
	ua := c.GetHeader("User-Agent")
	if err := db.UpsertSession(user.ID, tokenStr, ip, ua, expiresAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	maxAge := int(sessionExpiry.Seconds())
	c.SetCookie(cookieName, tokenStr, maxAge, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"redirect": "/courses"})
}

// ─── Password fallback ────────────────────────────────────────────────────────

// DoPasswordLogin — POST /login/password
// Fallback login used ONLY when Brevo email service is down.
// Returns JSON so the frontend JS can handle the response uniformly.
func DoPasswordLogin(c *gin.Context) {
	identifier := strings.TrimSpace(c.PostForm("username"))
	password   := c.PostForm("password")

	if identifier == "" || password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email/mobile and password are required."})
		return
	}

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials. Please try again."})
		return
	}
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "Account disabled. Contact admin."})
		return
	}
	if user.PasswordHash == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "No password set for this account. Use OTP login.",
		})
		return
	}
	if !CheckPassword(password, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials. Please try again."})
		return
	}

	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	ip := clientIP(c)
	ua := c.GetHeader("User-Agent")
	if err := db.UpsertSession(user.ID, tokenStr, ip, ua, expiresAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	maxAge := int(sessionExpiry.Seconds())
	c.SetCookie(cookieName, tokenStr, maxAge, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"redirect": "/courses"})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// generateOTP creates a cryptographically random 4-digit string (1000–9999).
// AFTER — cryptographically secure
func generateOTP() string {
    n, _ := rand.Int(rand.Reader, big.NewInt(9000))
    return fmt.Sprintf("%d", 1000+n.Int64())
}
