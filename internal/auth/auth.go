package auth

import (
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"golang.org/x/crypto/bcrypt"
)

// ─── JWT config ──────────────────────────────────────────────────────────────

var jwtSecret []byte

func init() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		// fallback — production must set JWT_SECRET
		secret = ""
	}
	jwtSecret = []byte(secret)
}

const (
	cookieName    = "fsb_session"
	sessionExpiry = 30 * 24 * time.Hour // 30 days
)

// ─── Claims ──────────────────────────────────────────────────────────────────

type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"uname"`
	jwt.RegisteredClaims
}

// ─── Token helpers ───────────────────────────────────────────────────────────

func issueToken(userID primitive.ObjectID, username string) (string, time.Time, error) {
	expiresAt := time.Now().Add(sessionExpiry)
	claims := &Claims{
		UserID:   userID.Hex(),
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(jwtSecret)
	return signed, expiresAt, err
}

func parseToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return nil, errors.New("invalid or expired token")
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	return claims, nil
}

// ─── Cookie security ─────────────────────────────────────────────────────────
// Production (DEV=false) मध्ये Secure=true — cookie फक्त HTTPS वरच जातो.
// Dev mode मध्ये Secure=false — localhost HTTP वर काम करतो.

func isSecureCookie() bool {
	return os.Getenv("DEV") != "true"
}

func setCookie(c *gin.Context, value string, maxAge int) {
	c.SetCookie(cookieName, value, maxAge, "/", "", isSecureCookie(), true)
}

func deleteCookie(c *gin.Context) {
	c.SetCookie(cookieName, "", -1, "/", "", isSecureCookie(), true)
}

// ─── Password helpers ────────────────────────────────────────────────────────

func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func CheckPassword(plain, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ─── clientIP ─────────────────────────────────────────────────────────────────

func clientIP(c *gin.Context) string {
	// respect X-Forwarded-For when behind a proxy / Cloudflare
	if ip := c.GetHeader("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	return c.ClientIP()
}

// ─── HTTP Handlers ────────────────────────────────────────────────────────────

// ShowLogin — GET /login
func ShowLogin(c *gin.Context) {
	// If already logged in redirect to courses
	if tok, err := c.Cookie(cookieName); err == nil && tok != "" {
		if claims, err := parseToken(tok); err == nil {
			if sess, _ := db.GetSessionByToken(tok); sess != nil {
				_ = claims
				c.Redirect(http.StatusFound, "/courses")
				return
			}
		}
	}
	c.HTML(http.StatusOK, "login.html", gin.H{"error": ""})
}

// DoLogin — POST /login
// Accepts mobile number, email, or username + password.
func DoLogin(c *gin.Context) {
	identifier := strings.TrimSpace(c.PostForm("username")) // field name kept "username" for HTML compat
	password := c.PostForm("password")

	fail := func(msg string) {
		c.HTML(http.StatusUnauthorized, "login.html", gin.H{"error": msg, "tab": "login"})
	}

	if identifier == "" || password == "" {
		fail("Mobile / Email and password are required.")
		return
	}

	// ── Block check — 4 wrong attempts = 5 hour lock ──────────────────────────
	if blocked, until := db.CheckLoginBlock(identifier); blocked {
		fail(fmt.Sprintf("Account locked until %s. Too many wrong attempts.", until.Format("3:04 PM, 02 Jan")))
		return
	}

	// Lookup by mobile, email, or username
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		// Record fail on unknown identifier too (prevents enumeration timing)
		db.RecordFailedLogin(identifier)
		fail("Invalid credentials. Please try again.")
		return
	}
	if !user.IsActive {
		fail("Your account has been disabled. Contact admin.")
		return
	}
	if !CheckPassword(password, user.PasswordHash) {
		_, msg := db.RecordFailedLogin(identifier)
		fail(msg)
		return
	}
	// ✓ Success — reset counter
	db.ResetLoginAttempts(identifier)

	// Issue JWT
	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
		fail("Server error. Please try again.")
		return
	}

	// One-session rule: UpsertSession kills all old sessions for this user first
	ip := clientIP(c)
	ua := c.GetHeader("User-Agent")
	if err := db.UpsertSession(user.ID, tokenStr, ip, ua, expiresAt); err != nil {
		fail("Server error. Please try again.")
		return
	}

	// Set secure cookie
	maxAge := int(sessionExpiry.Seconds())
	setCookie(c, tokenStr, maxAge)
	c.Redirect(http.StatusFound, "/courses")
}

// ShowRegister — GET /register
func ShowRegister(c *gin.Context) {
	// Already logged in? redirect
	if tok, err := c.Cookie(cookieName); err == nil && tok != "" {
		if _, err := parseToken(tok); err == nil {
			if sess, _ := db.GetSessionByToken(tok); sess != nil {
				c.Redirect(http.StatusFound, "/courses")
				return
			}
		}
	}
	c.HTML(http.StatusOK, "login.html", gin.H{"tab": "register", "error": ""})
}

// DoRegister — POST /register
// Step 1: Validate form, store pending record, send OTP to email.
// Returns JSON so the frontend can handle the response and show the OTP step.
func DoRegister(c *gin.Context) {
	fullName := strings.TrimSpace(c.PostForm("full_name"))
	mobile   := strings.TrimSpace(c.PostForm("mobile"))
	emailAddr := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	password  := c.PostForm("password")
	confirm   := c.PostForm("confirm_password")

	fail := func(msg string) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
	}

	// ── Validation ───────────────────────────────────────────────────────────
	if fullName == "" {
		fail("Full name is required.")
		return
	}
	if mobile == "" && emailAddr == "" {
		fail("Please enter your mobile number or email address.")
		return
	}
	if emailAddr == "" {
		fail("Email address is required for OTP verification.")
		return
	}
	if len(password) < 6 {
		fail("Password must be at least 6 characters.")
		return
	}
	if password != confirm {
		fail("Passwords do not match.")
		return
	}

	// ── Check duplicate ───────────────────────────────────────────────────────
	existing, _ := db.GetUserByMobileOrEmail(emailAddr)
	if existing != nil {
		fail("This email is already registered. Please sign in.")
		return
	}
	if mobile != "" {
		existingMobile, _ := db.GetUserByMobileOrEmail(mobile)
		if existingMobile != nil {
			fail("This mobile number is already registered.")
			return
		}
	}

	// ── Rate limit ────────────────────────────────────────────────────────────
	if !db.CanSendRegOTP(emailAddr) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Please wait 60 seconds before requesting a new OTP.",
		})
		return
	}

	// ── Hash password ─────────────────────────────────────────────────────────
	hash, err := HashPassword(password)
	if err != nil {
		fail("Server error. Please try again.")
		return
	}

	// ── Generate + hash OTP ───────────────────────────────────────────────────
	otpPlain := generateOTP()
	otpHash, err := HashPassword(otpPlain)
	if err != nil {
		fail("Server error. Please try again.")
		return
	}

	// ── Store pending registration ────────────────────────────────────────────
	if err := db.CreatePendingRegistration(emailAddr, mobile, fullName, hash, otpHash); err != nil {
		fail("Server error. Please try again.")
		return
	}

	// ── Send OTP email ────────────────────────────────────────────────────────
	if err := email.SendRegOTP(emailAddr, fullName, otpPlain); err != nil {
		log.Printf("[Register] SendRegOTP FAILED for %s: %v", emailAddr, err)
		_ = db.DeletePendingRegistration(emailAddr)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Failed to send verification email. Please try again.",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"step":  "otp",
		"email": emailAddr,
	})
}

// VerifyRegistrationOTP — POST /register/verify
// Step 2: Check OTP, create user, auto-login.
func VerifyRegistrationOTP(c *gin.Context) {
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

	// Fetch pending record
	rec, err := db.GetPendingRegistration(emailAddr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Please try again."})
		return
	}
	if rec == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OTP expired or not found. Please register again."})
		return
	}

	// ── Cumulative lockout check ──────────────────────────────────────────────
	// Registration OTP पण same lockout pool मध्ये येतो.
	// 4 cumulative wrong attempts (across any OTP requests) → 3h lock.
	if locked, until := db.CheckOTPLock(emailAddr); locked {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf(
				"Too many wrong OTP attempts. Try again after %s.",
				until.Format("3:04 PM, 02 Jan"),
			),
		})
		return
	}

	// Per-OTP attempt limit (max 3 tries on this specific OTP)
	if rec.Attempts >= maxOTPAttempts {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Too many wrong attempts. Please register again.",
		})
		return
	}

	// Verify OTP
	if !CheckPassword(otpCode, rec.OTPHash) {
		_ = db.IncrementPendingAttempts(rec.ID)

		// Cumulative failure count वाढवतो (cross-OTP tracking)
		nowLocked, lockMsg := db.RecordOTPLockFailure(emailAddr)
		if nowLocked {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": lockMsg})
			return
		}

		remaining := maxOTPAttempts - rec.Attempts - 1
		if remaining <= 0 {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Wrong OTP. No attempts left. Please register again.",
			})
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": fmt.Sprintf("Wrong OTP. %d attempt(s) remaining.", remaining),
			})
		}
		return
	}

	// ✓ OTP correct — reset lockout + create the user
	db.ResetOTPLock(emailAddr)
	user, err := db.CreateUserFull("", rec.FullName, rec.Mobile, emailAddr, rec.PasswordHash)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	// Clean up pending record
	_ = db.DeletePendingRegistration(emailAddr)

	// Auto-login
	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
		// Account created but token failed — redirect to login
		c.JSON(http.StatusOK, gin.H{"redirect": "/login?registered=1"})
		return
	}
	ip := clientIP(c)
	ua := c.GetHeader("User-Agent")
	_ = db.UpsertSession(user.ID, tokenStr, ip, ua, expiresAt)

	maxAge := int(sessionExpiry.Seconds())
	setCookie(c, tokenStr, maxAge)
	c.JSON(http.StatusOK, gin.H{"redirect": "/courses"})
}

// DoLogout — POST /logout
func DoLogout(c *gin.Context) {
	if tok, err := c.Cookie(cookieName); err == nil {
		_ = db.DeleteSession(tok)
	}
	deleteCookie(c)
	c.Redirect(http.StatusFound, "/login")
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// RequireAuth protects any route that needs a logged-in user.
// On failure it redirects to /login (browser) or returns 401 (API/fetch).
func RequireAuth(c *gin.Context) {
	tok, err := c.Cookie(cookieName)
	if err != nil || tok == "" {
		redirectOrUnauthorized(c)
		return
	}

	claims, err := parseToken(tok)
	if err != nil {
		clearCookie(c)
		redirectOrUnauthorized(c)
		return
	}

	// Verify session still exists in MongoDB
	// (catches the case where admin killed the session or a new login replaced it)
	sess, err := db.GetSessionByToken(tok)
	if err != nil || sess == nil {
		clearCookie(c)
		redirectOrUnauthorized(c)
		return
	}

	// Parse userID and store in context for downstream handlers
	userID, err := primitive.ObjectIDFromHex(claims.UserID)
	if err != nil {
		clearCookie(c)
		redirectOrUnauthorized(c)
		return
	}

	c.Set("userID", userID)
	c.Set("username", claims.Username)
	c.Set("sessionToken", tok)
	c.Next()
}

// ─── Context helpers (use in handlers after RequireAuth) ─────────────────────

func GetUserID(c *gin.Context) primitive.ObjectID {
	v, _ := c.Get("userID")
	id, _ := v.(primitive.ObjectID)
	return id
}

func GetUsername(c *gin.Context) string {
	v, _ := c.Get("username")
	s, _ := v.(string)
	return s
}

func GetSessionToken(c *gin.Context) string {
	v, _ := c.Get("sessionToken")
	s, _ := v.(string)
	return s
}

// ─── internal ─────────────────────────────────────────────────────────────────

func clearCookie(c *gin.Context) {
	deleteCookie(c)
}

func redirectOrUnauthorized(c *gin.Context) {
	// If the request is a fetch/XHR return JSON 401; otherwise redirect to login
	accept := c.GetHeader("Accept")
	if strings.Contains(accept, "application/json") ||
		c.GetHeader("X-Requested-With") == "XMLHttpRequest" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}
	c.Redirect(http.StatusFound, "/login")
	c.Abort()
}
