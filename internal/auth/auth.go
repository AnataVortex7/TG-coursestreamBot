package auth

import (
	"EverythingSuckz/fsb/internal/db"
	"errors"
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
		secret = "change-me-in-production-set-JWT_SECRET-env"
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

	// Lookup by mobile, email, or username
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		fail("Invalid credentials. Please try again.")
		return
	}
	if !user.IsActive {
		fail("Your account has been disabled. Contact admin.")
		return
	}
	if !CheckPassword(password, user.PasswordHash) {
		fail("Invalid credentials. Please try again.")
		return
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
	c.SetCookie(cookieName, tokenStr, maxAge, "/", "", false, true)
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
// User fills: full_name, mobile OR email, password, confirm_password.
func DoRegister(c *gin.Context) {
	fullName  := strings.TrimSpace(c.PostForm("full_name"))
	mobile    := strings.TrimSpace(c.PostForm("mobile"))
	email     := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	password  := c.PostForm("password")
	confirm   := c.PostForm("confirm_password")

	fail := func(msg string) {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{
			"tab":       "register",
			"error":     msg,
			"full_name": fullName,
			"mobile":    mobile,
			"email":     email,
		})
	}

	// Validation
	if fullName == "" {
		fail("Full name is required.")
		return
	}
	if mobile == "" && email == "" {
		fail("Please enter your mobile number or email address.")
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

	hash, err := HashPassword(password)
	if err != nil {
		fail("Server error. Please try again.")
		return
	}

	user, err := db.CreateUserFull("", fullName, mobile, email, hash)
	if err != nil {
		fail(err.Error()) // "mobile number or email already registered"
		return
	}

	// Auto-login after registration
	tokenStr, expiresAt, err := issueToken(user.ID, user.Username)
	if err != nil {
		// Registration succeeded but token failed — send to login
		c.Redirect(http.StatusFound, "/login?registered=1")
		return
	}
	ip := clientIP(c)
	ua := c.GetHeader("User-Agent")
	_ = db.UpsertSession(user.ID, tokenStr, ip, ua, expiresAt)

	maxAge := int(sessionExpiry.Seconds())
	c.SetCookie(cookieName, tokenStr, maxAge, "/", "", false, true)
	c.Redirect(http.StatusFound, "/courses")
}

// DoLogout — POST /logout
func DoLogout(c *gin.Context) {
	if tok, err := c.Cookie(cookieName); err == nil {
		_ = db.DeleteSession(tok)
	}
	c.SetCookie(cookieName, "", -1, "/", "", false, true)
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
	c.SetCookie(cookieName, "", -1, "/", "", false, true)
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
