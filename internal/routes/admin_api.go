// internal/routes/admin_api.go
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/commands"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
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

	// Sync — background goroutine using bot's Telegram client
	api.POST("/sync", adminSyncChannel)

	// Sessions
	api.GET("/sessions", adminListSessions)
	api.POST("/sessions/kill", adminKillSession)

	// Access — accepts user_id directly from dropdown
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
// SYNC CHANNEL — background goroutine, bot's Telegram client directly
// ═══════════════════════════════════════════════════════════════════════════════

func adminSyncChannel(c *gin.Context) {
	var req struct {
		CourseID string `json:"course_id"`
		Start    int    `json:"start"`
		End      int    `json:"end"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Course ID required"})
		return
	}
	if req.Start < 1 || req.End < req.Start {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Start ≥ 1 ani End ≥ Start pahije"})
		return
	}

	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Course not found"})
		return
	}
	if course.ChannelID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ya course la Telegram channel set nahi"})
		return
	}

	client := commands.MainBotClient
	if client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Bot client ready nahi — bot started aahe ka check kara"})
		return
	}

	startID := req.Start
	endID := req.End

	// Background goroutine — API immediately return karto
	go func() {
		ctx := context.Background()
		const batchSize = 100

		channel, err := utils.GetChannelPeer(ctx, client.API(), client.PeerStorage, course.ChannelID)
		if err != nil {
			log.Printf("[AdminSync] GetChannelPeer failed for %s: %v", course.Title, err)
			return
		}

		total := 0
		skipped := 0

		for batchStart := startID; batchStart <= endID; batchStart += batchSize {
			batchEnd := batchStart + batchSize - 1
			if batchEnd > endID {
				batchEnd = endID
			}

			ids := make([]tg.InputMessageClass, 0, batchEnd-batchStart+1)
			for id := batchStart; id <= batchEnd; id++ {
				ids = append(ids, &tg.InputMessageID{ID: id})
			}

			res, fetchErr := client.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: channel,
				ID:      ids,
			})
			if fetchErr != nil {
				log.Printf("[AdminSync] Fetch failed: %v", fetchErr)
				break
			}

			messages, ok := res.(*tg.MessagesChannelMessages)
			if !ok {
				log.Printf("[AdminSync] Unexpected Telegram response type")
				break
			}

			for _, rawMsg := range messages.Messages {
				msg, ok := rawMsg.(*tg.Message)
				if !ok || msg.Media == nil {
					skipped++
					continue
				}

				file, err := utils.FileFromMedia(msg.Media)
				if err != nil {
					skipped++
					continue
				}

				caption := strings.TrimSpace(msg.Message)
				title := caption
				if title == "" {
					title = adminStripExt(file.FileName)
				}

				sortOrder, _ := db.HighestSortOrder(courseID)
				cached := &db.CachedFile{
					MessageID: msg.ID,
					ChannelID: course.ChannelID,
					CourseID:  courseID,
					Title:     title,
					Caption:   caption,
					FileName:  file.FileName,
					MimeType:  file.MimeType,
					FileSize:  file.FileSize,
					SortOrder: sortOrder + 1,
					CreatedAt: time.Now(),
				}
				if err := db.UpsertCachedFile(cached); err != nil {
					skipped++
					continue
				}
				total++
			}

			// Rate limit टाळण्यासाठी pause
			time.Sleep(200 * time.Millisecond)
		}

		log.Printf("[AdminSync] ✅ Done — Course: %s | Range: %d→%d | Cached: %d | Skipped: %d",
			course.Title, startID, endID, total, skipped)
	}()

	c.JSON(http.StatusOK, gin.H{
		"status":  "started",
		"message": "Sync background madhe chalu zali! Thodyach velat files count update hoil.",
		"course":  course.Title,
	})
}

// adminStripExt — filename madhun extension kadhto
func adminStripExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
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
	Token     string `json:"token"`      // full token — for kill
	TokenHint string `json:"token_hint"` // last 8 chars — display only
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
// ACCESS CONTROL — user_id aani course_id dropdown madhuun directly येतात
// ═══════════════════════════════════════════════════════════════════════════════

func adminGrantAccess(c *gin.Context) {
	var req struct {
		UserID   string `json:"user_id"`
		CourseID string `json:"course_id"`
		Months   int    `json:"months"` // 0 = permanent
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.UserID == "" || req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "User ani Course select kara"})
		return
	}

	userID, err := primitive.ObjectIDFromHex(req.UserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	if err := db.GrantAccess(userID, courseID, 0, req.Months); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": user.Username, "full_name": user.FullName})
}

func adminRevokeAccess(c *gin.Context) {
	var req struct {
		UserID   string `json:"user_id"`
		CourseID string `json:"course_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.UserID == "" || req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "User ani Course select kara"})
		return
	}

	userID, err := primitive.ObjectIDFromHex(req.UserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	if err := db.RevokeAccess(userID, courseID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": user.Username, "full_name": user.FullName})
}
