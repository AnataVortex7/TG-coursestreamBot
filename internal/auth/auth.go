package auth

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"errors"
	"fmt"
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

// NotifyAdmins — registration zalyavar Telegram var allowed admins la DM
// pathavayla call hoto. bot package madhun run.go ne set kela jato (import
// cycle टाळayla): bot already commands import karte, commands auth import
// karte, mhanun auth ne bot direct import karu shakat nahi (cycle: bot →
// commands → auth → bot). Same pattern jasं commands.SyncChannelAdmins var
// vaaparla aahe. Nil असेल (server suru hotanach set nahi zala) tar caller ne
// skip karaycha.
var NotifyAdmins func(text string)

func init() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		// JWT_SECRET not set — empty secret means ANYONE can forge valid tokens.
		// Production वर हे critical vulnerability आहे.
		// Server start होणे block करतो — silent failure पेक्षा panic better.
		panic("FATAL: JWT_SECRET environment variable is not set. " +
			"Set a strong random secret (min 32 chars) before running in production. " +
			"Example: openssl rand -hex 32")
	}
	if len(secret) < 32 {
		// Weak secret — brute-force attack शक्य आहे
		panic("FATAL: JWT_SECRET must be at least 32 characters long. " +
			"Generate one with: openssl rand -hex 32")
	}
	jwtSecret = []byte(secret)
}

const (
	cookieName    = "ak_session"
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
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   isSecureCookie(),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func deleteCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   isSecureCookie(),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
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

	// ── Device fingerprint check ───────────────────────────────────────────────
	resolution := c.PostForm("fp_res")
	timezone   := c.PostForm("fp_tz")
	if CheckAndSetFingerprint(c, user, resolution, timezone, false) {
		return // blocked — redirect already sent
	}

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
	c.HTML(http.StatusOK, "login.html", gin.H{
		"tab":               "register",
		"error":             "",
		"registration_open": db.GetSettingBool(db.SettingRegistrationOpen, true),
	})
}

// DoRegister — POST /register
// Validate form, create user directly, auto-login (no OTP step).
func DoRegister(c *gin.Context) {
	// ── Registration open/close gate ───────────────────────────────────────────
	// Ha check sagळ्याच्या aadhi — form parse honya, validation honya, ki DB
	// query honya AADHI. Admin ne registration OFF kelela asel tar koणीही
	// (UI bypass karun direct POST /register hit kela tarihi, curl/Postman
	// varun access kela tarihi) navin account create karu shaknar nahi.
	// Frontend cha "Registration Closed" banner ha फक्त UX साठी आहे —
	// khara enforcement fakt ithech, server-side, honar.
	if !db.GetSettingBool(db.SettingRegistrationOpen, true) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "New registrations are currently closed. Please try again later.",
		})
		return
	}

	fullName  := strings.TrimSpace(c.PostForm("full_name"))
	mobile    := strings.TrimSpace(c.PostForm("mobile"))
	emailAddr := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	password  := c.PostForm("password")
	confirm   := c.PostForm("confirm_password")

	fail := func(msg string) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
	}

	if fullName == "" {
		fail("Full name is required.")
		return
	}
	if emailAddr == "" {
		fail("Email address is required.")
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

	hash, err := HashPassword(password)
	if err != nil {
		fail("Server error. Please try again.")
		return
	}

	user, err := db.CreateUserFull("", fullName, mobile, emailAddr, hash)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	// Admin la naveen registration cha mail - jo paryant admin "Special User"
	// ON karat nahi, tya user la courses cha fakt buy-now page disel.
	go func() {
		if config.ValueOf.AdminEmail == "" {
			return
		}
		_ = email.SendRegistrationRequest(config.ValueOf.AdminEmail, emailAddr, fullName, 0)
	}()

	// Email sobat Telegram var pn — allowed admin users la
	if NotifyAdmins != nil {
		go NotifyAdmins(fmt.Sprintf(
			"🆕 New User Registered\n\nName: %s\nMobile: %s\nEmail: %s\n\nJo paryant Special User ON karat nahi, tyala fakt buy-now page disel.",
			fullName, mobile, emailAddr,
		))
	}

	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
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

// ─── Admin Impersonate helper ─────────────────────────────────────────────────

// IssueTokenForUser — admin panel साठी: कोणत्याही user साठी JWT + session बनवतो.
// Admin ला त्या user म्हणून browse करण्यासाठी हे token cookie मध्ये set करायचे.
// User ला कळत नाही; त्याचा existing session kill होत नाही.
func IssueTokenForUser(c *gin.Context, userID primitive.ObjectID, username string) error {
	tokenStr, expiresAt, err := issueToken(userID, username)
	if err != nil {
		return err
	}
	ip := clientIP(c)
	ua := "admin-impersonate"
	if err := db.CreateImpersonateSession(userID, tokenStr, ip, ua, expiresAt); err != nil {
		return err
	}
	maxAge := int(sessionExpiry.Seconds())
	setCookie(c, tokenStr, maxAge)
	return nil
}
