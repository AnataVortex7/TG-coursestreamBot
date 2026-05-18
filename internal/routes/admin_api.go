// internal/routes/admin_api.go
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Admin credentials from env ──────────────────────────────────────────────
// Set ADMIN_USER and ADMIN_PASS in your .env file.
// Defaults to Akshay / Akshay1181 if not set.

func adminUser() string {
	if v := os.Getenv("ADMIN_USER"); v != "" {
		return v
	}
	return "Akshay"
}

func adminPass() string {
	if v := os.Getenv("ADMIN_PASS"); v != "" {
		return v
	}
	return "Akshay1181"
}

// ─── Basic-Auth middleware ────────────────────────────────────────────────────

func adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Basic ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 || parts[0] != adminUser() || parts[1] != adminPass() {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ─── Register all admin API routes ───────────────────────────────────────────

func (e *allRoutes) LoadAdminAPI(r *Route) {
	api := r.Engine.Group("/api/admin", adminAuthMiddleware())

	// Health ping — used by frontend to verify login
	api.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Stats
	api.GET("/stats", adminStats)

	// Users
	api.GET("/users", adminListUsers)
	api.POST("/users", adminAddUser)
	api.DELETE("/users/:id", adminDeleteUser)

	// Courses
	api.GET("/courses", adminListCourses)
	api.POST("/courses", adminAddCourse)
	api.DELETE("/courses/:id", adminDeleteCourse)

	// Sessions
	api.GET("/sessions", adminListSessions)
	api.POST("/sessions/kill", adminKillSession)

	// Access
	api.POST("/access/grant", adminGrantAccess)
	api.POST("/access/revoke", adminRevokeAccess)
}

// ═══════════════════════════════════════════════════════════════════════════════
// STATS
// ═══════════════════════════════════════════════════════════════════════════════

func adminStats(c *gin.Context) {
	users, _ := db.ListUsers()
	courses, _ := db.ListAllCourses()
	sessions, _ := db.ListActiveSessions()

	activeUsers := 0
	for _, u := range users {
		if u.IsActive {
			activeUsers++
		}
	}

	totalFiles := int64(0)
	for _, co := range courses {
		n, _ := db.CountCachedFiles(co.ID)
		totalFiles += n
	}

	c.JSON(http.StatusOK, gin.H{
		"users":        len(users),
		"active_users": activeUsers,
		"courses":      len(courses),
		"files":        totalFiles,
		"sessions":     len(sessions),
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// USERS
// ═══════════════════════════════════════════════════════════════════════════════

func adminListUsers(c *gin.Context) {
	users, err := db.ListUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, users)
}

func adminAddUser(c *gin.Context) {
	var req struct {
		FullName string `json:"full_name"`
		Mobile   string `json:"mobile"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Confirm  string `json:"confirm_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	req.FullName = strings.TrimSpace(req.FullName)
	req.Mobile = strings.TrimSpace(req.Mobile)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	if req.FullName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Full name is required"})
		return
	}
	if req.Mobile == "" && req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mobile number or email is required"})
		return
	}
	if len(req.Password) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password must be at least 6 characters"})
		return
	}
	if req.Password != req.Confirm {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Passwords do not match"})
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process password"})
		return
	}

	user, err := db.CreateUserFull("", req.FullName, req.Mobile, req.Email, hash)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, user)
}

func adminDeleteUser(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	if err := db.SetUserActive(id, false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Kill all active sessions for this user
	_ = db.DeleteAllSessionsForUser(id)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ═══════════════════════════════════════════════════════════════════════════════
// COURSES
// ═══════════════════════════════════════════════════════════════════════════════

type courseWithFiles struct {
	db.Course
	Files int64 `json:"files"`
}

func adminListCourses(c *gin.Context) {
	courses, err := db.ListAllCourses()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result := make([]courseWithFiles, 0, len(courses))
	for _, co := range courses {
		n, _ := db.CountCachedFiles(co.ID)
		result = append(result, courseWithFiles{Course: co, Files: n})
	}
	c.JSON(http.StatusOK, result)
}

func adminAddCourse(c *gin.Context) {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Thumbnail   string `json:"thumbnail"`
		Price       string `json:"price"`
		BuyLink     string `json:"buy_link"`
		ChannelID   int64  `json:"channel_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if strings.TrimSpace(req.Title) == "" || req.ChannelID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title and Channel ID are required"})
		return
	}
	course, err := db.CreateCourse(req.Title, req.Description, req.Thumbnail, req.Price, req.BuyLink, req.ChannelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, course)
}

func adminDeleteCourse(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}
	if err := db.DeleteCourse(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = db.DeleteCachedFilesForCourse(id)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ═══════════════════════════════════════════════════════════════════════════════
// SESSIONS
// ═══════════════════════════════════════════════════════════════════════════════

type sessionRow struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FullName  string `json:"full_name"`
	Mobile    string `json:"mobile"`
	Email     string `json:"email"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
	ExpiresAt string `json:"expires_at"`
	Token     string `json:"token"` // full token — used for kill request
	TokenHint string `json:"token_hint"` // last 8 chars — for display only
}

func adminListSessions(c *gin.Context) {
	sessions, err := db.ListActiveSessions()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	rows := make([]sessionRow, 0, len(sessions))
	for _, s := range sessions {
		u, _ := db.GetUserByID(s.UserID)
		username, fullName, mobile, email := s.UserID.Hex()[:8]+"...", "", "", ""
		if u != nil {
			username = u.Username
			fullName = u.FullName
			mobile = u.Mobile
			email = u.Email
		}
		hint := s.Token
		if len(hint) > 8 {
			hint = "..." + hint[len(hint)-8:]
		}
		rows = append(rows, sessionRow{
			ID:        s.ID.Hex(),
			Username:  username,
			FullName:  fullName,
			Mobile:    mobile,
			Email:     email,
			IP:        s.IP,
			UserAgent: s.UserAgent,
			ExpiresAt: s.ExpiresAt.Format("02 Jan 2006 15:04"),
			Token:     s.Token,
			TokenHint: hint,
		})
	}
	c.JSON(http.StatusOK, rows)
}

func adminKillSession(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Token required"})
		return
	}
	if err := db.DeleteSession(req.Token); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ═══════════════════════════════════════════════════════════════════════════════
// ACCESS CONTROL
// ═══════════════════════════════════════════════════════════════════════════════

func adminGrantAccess(c *gin.Context) {
	var req struct {
		Identifier string `json:"identifier"` // mobile or email
		CourseID   string `json:"course_id"`
		Months     int    `json:"months"` // 0 = permanent
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.Identifier == "" || req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Identifier and Course ID are required"})
		return
	}

	user, err := db.GetUserByMobileOrEmail(strings.ToLower(strings.TrimSpace(req.Identifier)))
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found with this mobile/email"})
		return
	}

	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	if err := db.GrantAccess(user.ID, courseID, 0, req.Months); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": user.Username})
}

func adminRevokeAccess(c *gin.Context) {
	var req struct {
		Identifier string `json:"identifier"`
		CourseID   string `json:"course_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.Identifier == "" || req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Identifier and Course ID are required"})
		return
	}

	user, err := db.GetUserByMobileOrEmail(strings.ToLower(strings.TrimSpace(req.Identifier)))
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found with this mobile/email"})
		return
	}

	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	if err := db.RevokeAccess(user.ID, courseID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": user.Username})
}

// ─── unused import guard ──────────────────────────────────────────────────────
var _ = context.Background
var _ = bson.M{}
var _ = time.Now
