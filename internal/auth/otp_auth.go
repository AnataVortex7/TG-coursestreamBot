// internal/auth/otp_auth.go
// OTP-based Telegram login handlers + password fallback.
// OTP आता फक्त Telegram वरून पाठवला जातो — email OTP पूर्ण काढून टाकला आहे.
// These sit alongside the existing auth.go handlers in the same package.
package auth

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const maxOTPAttempts = 3

var ist = time.FixedZone("IST", 5*60*60+30*60)

// SendTelegramOTP — bot ला telegram msg पाठवण्याचं actual काम करतो.
// इथून थेट "internal/bot" import केलं तर import cycle होतो
// (bot → commands → auth), म्हणून dependency injection वापरलं आहे —
// run.go मध्ये startup ला `auth.SendTelegramOTP = bot.SendOTPToTelegram` असं set होतं.
var SendTelegramOTP func(telegramID int64, otpPlain string) error

// ─── Send OTP ─────────────────────────────────────────────────────────────────

// SendOTPHandler — POST /login/otp/send
// Body (form): email=<mobile_or_email identifier>
//
// OTP आता फक्त Telegram वर पाठवला जातो, कधीच email वर नाही:
//   - Telegram आधीच linked (TelegramID != 0) असेल → OTP थेट त्या chat वर.
//   - Linked नसेल → एक deep-link token देतो, frontend "Telegram उघडा" बटण दाखवतो.
//     User bot मध्ये /start दाबताच auto-link होतो आणि लगेच OTP telegram वर जातो.
//
// Security notes:
//   - Identifier registered नसेल तरी generic success response (user enumeration टाळण्यासाठी).
//   - Rate-limited: 1 OTP / 60s (linked) किंवा 1 deep-link / 30s (not linked).
//   - OTP bcrypt hash म्हणूनच store होतो, कधीच plain text नाही.
func SendOTPHandler(c *gin.Context) {
	identifier := strings.ToLower(strings.TrimSpace(c.PostForm("email")))

	if identifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mobile number or email is required."})
		return
	}

	// ── Cumulative lockout check ──────────────────────────────────────────────
	if locked, until := db.CheckOTPLock(identifier); locked {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf(
				"Too many wrong OTP attempts. Try again after %s.",
				until.In(ist).Format("3:04 PM IST, 02 Jan"),
			),
		})
		return
	}

	// Verify user exists and is active (but don't reveal which is the case to caller)
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		// Generic response — don't reveal non-existence
		c.JSON(http.StatusOK, gin.H{"message": "If this account is registered, you will receive an OTP on Telegram shortly."})
		return
	}
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "Your account is disabled. Contact admin."})
		return
	}

	// ── Case A: Telegram आधीच linked ──────────────────────────────────────────
	if user.TelegramID != 0 {
		if !db.CanSendOTP(identifier) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Please wait 60 seconds before requesting a new OTP.",
			})
			return
		}

		otpPlain := generateOTP()
		otpHash, err := HashPassword(otpPlain)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
			return
		}
		if err := db.CreateOTP(identifier, otpHash); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
			return
		}

		if SendTelegramOTP == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Server misconfigured. Contact admin."})
			return
		}
		if err := SendTelegramOTP(user.TelegramID, otpPlain); err != nil {
			log.Printf("[OTP] Telegram send FAILED for %s (tg:%d): %v", identifier, user.TelegramID, err)
			// रेट-लिमिट block होऊ नये म्हणून record काढून टाकतो, user लगेच retry करू शकेल.
			_ = db.DeleteOTP(identifier)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Failed to send OTP on Telegram. Please open the bot once and try again.",
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"message": "OTP sent to your Telegram account.",
			"channel": "telegram",
		})
		return
	}

	// ── Case B: Telegram अजून linked नाही → deep-link token ───────────────────
	if !db.CanCreateTGLoginToken(user.ID) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Please wait a bit before requesting a new Telegram link.",
		})
		return
	}
	if config.ValueOf.BotUsername == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server misconfigured (BOT_USERNAME not set). Contact admin."})
		return
	}

	token, err := db.CreateTGLoginToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	deepLink := fmt.Sprintf("https://t.me/%s?start=totp_%s", config.ValueOf.BotUsername, token)

	c.JSON(http.StatusOK, gin.H{
		"need_telegram_link": true,
		"deep_link":          deepLink,
		"message":            "Open Telegram and press Start. Complete the quick phone-number verification there, then come back here and request the OTP again.",
	})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mobile/email and OTP are required."})
		return
	}
	if len(otpCode) != 4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTP must be exactly 4 digits."})
		return
	}

	// Serialise attempts per identifier — parallel guesses must not all pass the
	// attempt-counter / lockout checks before the first failure is recorded.
	unlock := lockIdentifier(emailAddr)
	defer unlock()

	// ── Cumulative lockout check ──────────────────────────────────────────────
	// हे check per-OTP attempt limit (3) आधी येतो.
	// 4 cumulative wrong attempts → 3h lock, नवीन OTP request पण block.
	if locked, until := db.CheckOTPLock(emailAddr); locked {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf(
				"Too many wrong OTP attempts. Try again after %s.",
				until.In(ist).Format("3:04 PM IST, 02 Jan"),
			),
		})
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

	// Per-OTP attempt limit (max 3 tries on this specific OTP)
	if rec.Attempts >= maxOTPAttempts {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Too many wrong attempts. Please request a new OTP.",
		})
		return
	}

	// Verify OTP (bcrypt compare)
	if !CheckPassword(otpCode, rec.OTPHash) {
		_ = db.IncrementOTPAttempts(rec.ID)

		// Cumulative failure count वाढवतो (cross-OTP tracking)
		nowLocked, lockMsg := db.RecordOTPLockFailure(emailAddr)
		if nowLocked {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": lockMsg})
			return
		}

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

	// ✓ OTP correct — consume it and reset lockout
	_ = db.MarkOTPUsed(rec.ID)
	db.ResetOTPLock(emailAddr)

	// Fetch the user record to issue a session
	user, err := db.GetUserByMobileOrEmail(emailAddr)
	if err != nil || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	// ── Device fingerprint check ───────────────────────────────────────────────
	if CheckAndSetFingerprint(c, user, c.PostForm("fp_res"), c.PostForm("fp_tz"), true) {
		return // blocked — JSON redirect already sent
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
	setCookie(c, tokenStr, maxAge)
	c.JSON(http.StatusOK, gin.H{"redirect": "/courses"})
}

// ─── Password fallback ────────────────────────────────────────────────────────

// DoPasswordLogin — POST /login/password
// Alternate login method — useful when Telegram OTP is not reachable
// (bot down, user hasn't opened Telegram, etc).
// Returns JSON so the frontend JS can handle the response uniformly.
func DoPasswordLogin(c *gin.Context) {
	identifier := strings.TrimSpace(c.PostForm("username"))
	password   := c.PostForm("password")

	if identifier == "" || password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email/mobile and password are required."})
		return
	}

	unlock := lockIdentifier(identifier)
	defer unlock()

	// ── Block check ─────────────────────────────────────────────────────────────
	if blocked, until := db.CheckLoginBlock(identifier); blocked {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf(
				"Account locked until %s. Too many wrong attempts.",
				until.In(ist).Format("3:04 PM IST, 02 Jan"),
			),
		})
		return
	}

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		_ = CheckPassword(password, dummyHash) // equalise timing with the "user exists" path
		db.RecordFailedLogin(identifier)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials. Please try again."})
		return
	}
	if user.PasswordHash == "" {
		_ = CheckPassword(password, dummyHash)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "No password set for this account. Use OTP login.",
		})
		return
	}
	if !CheckPassword(password, user.PasswordHash) {
		_, msg := db.RecordFailedLogin(identifier)
		c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
		return
	}
	// Status is revealed only AFTER the password is proven correct.
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "Account disabled. Contact admin."})
		return
	}
	// ✓ Success — reset counter
	db.ResetLoginAttempts(identifier)

	// ── Device fingerprint check ───────────────────────────────────────────────
	if CheckAndSetFingerprint(c, user, c.PostForm("fp_res"), c.PostForm("fp_tz"), true) {
		return // blocked — JSON redirect already sent
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
	setCookie(c, tokenStr, maxAge)
	c.JSON(http.StatusOK, gin.H{"redirect": "/courses"})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// generateOTP creates a cryptographically random 4-digit string (1000–9999).
func generateOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(9000))
	return fmt.Sprintf("%d", 1000+n.Int64())
}
