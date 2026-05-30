// internal/routes/admin_api.go
package routes

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/commands"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"EverythingSuckz/fsb/internal/urlmonitor"
	"EverythingSuckz/fsb/internal/utils"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	fmt "fmt"
	golog "log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Admin credentials from env ──────────────────────────────────────────────
// Set ADMIN_USER and ADMIN_PASS in your .env file.
// Defaults to  if not set.

func adminUser() string {
	if v := os.Getenv("ADMIN_USER"); v != "" {
		return v
	}
	return ""
}

func adminPass() string {
	if v := os.Getenv("ADMIN_PASS"); v != "" {
		return v
	}
	return ""
}

// ─── Admin session store (in-memory, HttpOnly cookie) ────────────────────────

var (
	adminSessions   sync.Map // token(string) → expiry(time.Time)
	adminCookieName = "fsb_admin"
	adminSessionTTL = 8 * time.Hour
)

func genAdminToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ─── Brute Force Limiter ──────────────────────────────────────────────────────
// Per-IP: max 5 failed attempts in 15 min → 30 min lockout.

type loginAttempt struct {
	failures  int
	lastFail  time.Time
	lockedAt  time.Time
}

var (
	loginAttempts   sync.Map // ip(string) → *loginAttempt
	maxFailures     = 5
	window          = 15 * time.Minute
	lockoutDuration = 30 * time.Minute
)

func checkBruteForce(ip string) (locked bool, remaining time.Duration) {
	v, _ := loginAttempts.LoadOrStore(ip, &loginAttempt{})
	a := v.(*loginAttempt)

	// Lockout active?
	if !a.lockedAt.IsZero() {
		unlockAt := a.lockedAt.Add(lockoutDuration)
		if time.Now().Before(unlockAt) {
			return true, time.Until(unlockAt)
		}
		// Lockout expired — reset
		a.failures = 0
		a.lockedAt = time.Time{}
	}

	// Window expired — reset counter
	if time.Since(a.lastFail) > window {
		a.failures = 0
	}
	return false, 0
}

func recordFailure(ip string) {
	v, _ := loginAttempts.LoadOrStore(ip, &loginAttempt{})
	a := v.(*loginAttempt)
	a.failures++
	a.lastFail = time.Now()
	if a.failures >= maxFailures {
		a.lockedAt = time.Now()
		log.Warn(fmt.Sprintf("Admin brute force lockout: IP %s (%d failures)", ip, a.failures))
	}
}

func recordSuccess(ip string) {
	loginAttempts.Delete(ip) // success → counter reset
}

// ─── Admin IP whitelist (optional) ───────────────────────────────────────────
// ADMIN_ALLOWED_IPS="1.2.3.4,5.6.7.8" set केल्यास फक्त त्याच IPs ला login होतो.
// Not set = all IPs allowed.

func allowedAdminIPs() []string {
	v := os.Getenv("ADMIN_ALLOWED_IPS")
	if v == "" {
		return nil
	}
	var ips []string
	for _, ip := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(ip); trimmed != "" {
			ips = append(ips, trimmed)
		}
	}
	return ips
}

func isIPAllowed(ip string) bool {
	allowed := allowedAdminIPs()
	if len(allowed) == 0 {
		return true // whitelist नाही — सर्व allow
	}
	for _, a := range allowed {
		if a == ip {
			return true
		}
	}
	return false
}

// adminLogin — POST /api/admin/login
func adminLogin(c *gin.Context) {
	ip := c.ClientIP()

	// ── 1. IP whitelist check ─────────────────────────────────────────────────
	if !isIPAllowed(ip) {
		log.Warn(fmt.Sprintf("Admin login blocked: IP %s not in whitelist", ip))
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// ── 2. Brute force check ──────────────────────────────────────────────────
	if locked, remaining := checkBruteForce(ip); locked {
		mins := int(remaining.Minutes()) + 1
		log.Warn(fmt.Sprintf("Admin login blocked (lockout): IP %s, %dm remaining", ip, mins))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", mins),
		})
		return
	}

	// ── 3. Credentials ────────────────────────────────────────────────────────
	u := strings.TrimSpace(c.PostForm("user"))
	p := c.PostForm("pass")
	if u == "" || p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user ani pass required"})
		return
	}

	// Constant-time comparison — timing attack नाही
	uOK := subtle.ConstantTimeCompare([]byte(u), []byte(adminUser())) == 1
	pOK := subtle.ConstantTimeCompare([]byte(p), []byte(adminPass())) == 1

	if !uOK || !pOK {
		recordFailure(ip)
		v, _ := loginAttempts.Load(ip)
		a := v.(*loginAttempt)
		left := maxFailures - a.failures
		log.Warn(fmt.Sprintf("Admin login failed: IP %s, user=%q, attempts_left=%d", ip, u, left))
		if left <= 0 {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": fmt.Sprintf("Too many failed attempts. Locked for %d minutes.", int(lockoutDuration.Minutes())),
			})
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": fmt.Sprintf("Invalid credentials. %d attempts remaining.", left),
			})
		}
		return
	}

	// ── 4. Success ────────────────────────────────────────────────────────────
	recordSuccess(ip)

	// Single active session — जुने sessions invalidate करतो
	adminSessions.Range(func(k, _ any) bool {
		adminSessions.Delete(k)
		return true
	})

	tok := genAdminToken()
	adminSessions.Store(tok, time.Now().Add(adminSessionTTL))

	// Secure=true in production (HTTPS), false in dev
	secure := !config.ValueOf.Dev
	c.SetCookie(adminCookieName, tok, int(adminSessionTTL.Seconds()), "/", "", secure, true)

	log.Info(fmt.Sprintf("Admin login success: IP %s", ip))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// adminLogout — POST /api/admin/logout
func adminLogout(c *gin.Context) {
	if tok, err := c.Cookie(adminCookieName); err == nil {
		adminSessions.Delete(tok)
	}
	c.SetCookie(adminCookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// adminAuthMiddleware — session check + Origin validation + CSRF (for mutating requests).
func adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// ── 1. Session cookie check ───────────────────────────────────────────
		tok, err := c.Cookie(adminCookieName)
		if err != nil || tok == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		val, ok := adminSessions.Load(tok)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired"})
			c.Abort()
			return
		}
		if time.Now().After(val.(time.Time)) {
			adminSessions.Delete(tok)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired"})
			c.Abort()
			return
		}

		// ── 2. Origin header validation (#6) ──────────────────────────────────
		// Browser cross-origin requests मध्ये Origin header येतो.
		// Same-origin requests किंवा non-browser clients मध्ये Origin नसतो — allow.
		if origin := c.GetHeader("Origin"); origin != "" {
			if !isAllowedOrigin(origin) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Invalid origin"})
				c.Abort()
				return
			}
		}

		// ── 3. CSRF check for mutating methods (#2) ───────────────────────────
		// CSRFRequired middleware इथे delegate केला —
		// admin group च्या सर्व POST/DELETE/PUT/PATCH requests validate होतात.
		auth.CSRFRequired(c)
		if c.IsAborted() {
			return
		}

		c.Next()
	}
}

// isAllowedOrigin — Origin header config च्या host शी match करतो.
// Dev mode मध्ये localhost/127.0.0.1 पण allow करतो.
func isAllowedOrigin(origin string) bool {
	host := strings.TrimRight(config.ValueOf.Host, "/")
	if strings.EqualFold(origin, host) {
		return true
	}
	// Dev mode — localhost allow
	if config.ValueOf.Dev {
		return strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1")
	}
	return false
}

// ─── Register all admin API routes ───────────────────────────────────────────

func (e *allRoutes) LoadAdminAPI(r *Route) {
	// Public endpoints — no auth required
	r.Engine.POST("/api/admin/login", adminLogin)
	r.Engine.POST("/api/admin/logout", adminAuthMiddleware(), adminLogout)

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

	// Messaging — admin user la message pathavto
	api.POST("/send-message", adminSendMessageToUser)

	// ─── Broadcast ───────────────────────────────────────────────────────────
	api.POST("/broadcast", adminBroadcast)
	api.GET("/announcements", adminListAnnouncements)
	api.DELETE("/announcements/:id", adminDeleteAnnouncement)

	// ─── URL Monitor ─────────────────────────────────────────────────────────
	api.GET("/urlmonitor", adminURLMonitorList)
	api.POST("/urlmonitor", adminURLMonitorAdd)
	api.DELETE("/urlmonitor/:id", adminURLMonitorRemove)
	api.GET("/urlmonitor/:id/history", adminURLMonitorHistory)
	api.POST("/urlmonitor/:id/check", adminURLMonitorCheck)

	// ── Workers (admin panel) ─────────────────────────────────────────────────
	// adminAuthMiddleware() automatically applied via the api group above.
	// Normal users cannot reach these — admin session + CSRF + Origin required.
	api.GET("/worker/list", handleAdminWorkerList)
	api.POST("/worker/add", handleAdminWorkerAdd)
	api.DELETE("/worker/remove", handleAdminWorkerRemove)

	// ── Live streams — admin panel real-time stream tracking ──────────────
	api.GET("/stream/live", handleAdminLiveStreams)

	// ── User password change & impersonate ────────────────────────────────
	api.PUT("/users/:id/password", adminChangeUserPassword)
	api.POST("/users/:id/impersonate", adminImpersonateUser)
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

	// Background goroutine: sarv registered users la "New Course" email pathav
	go func(courseTitle, courseBuyLink, coursePrice string) {
		users, err := db.ListUsers()
		if err != nil {
			golog.Printf("[NewCourse] ListUsers error: %v", err)
			return
		}
		appName := os.Getenv("BREVO_FROM_NAME")
		if appName == "" {
			appName = "ONLINE COURSE"
		}
		sent, skipped := 0, 0
		for _, u := range users {
			if u.Email == "" {
				skipped++
				continue
			}
			if err := email.SendNewCourseNotification(u.Email, u.FullName, courseTitle, courseBuyLink, coursePrice, appName); err != nil {
				golog.Printf("[NewCourse] Email to %s failed: %v", u.Email, err)
			} else {
				sent++
			}
		}
		golog.Printf("[NewCourse] '%s' — email pathavla: %d, skipped (no email): %d", courseTitle, sent, skipped)
	}(course.Title, course.BuyLink, course.Price)

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
			golog.Printf("[AdminSync] GetChannelPeer failed for %s: %v", course.Title, err)
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
				golog.Printf("[AdminSync] Fetch failed: %v", fetchErr)
				break
			}

			messages, ok := res.(*tg.MessagesChannelMessages)
			if !ok {
				golog.Printf("[AdminSync] Unexpected Telegram response type")
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

		golog.Printf("[AdminSync] ✅ Done — Course: %s | Range: %d→%d | Cached: %d | Skipped: %d",
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

// ═══════════════════════════════════════════════════════════════════════════════
// URL MONITOR
// ═══════════════════════════════════════════════════════════════════════════════

// adminURLMonitorList — GET /api/admin/urlmonitor
// Returns live status of all monitored URLs
func adminURLMonitorList(c *gin.Context) {
	statuses := urlmonitor.ListAllStatus()
	if statuses == nil {
		statuses = []urlmonitor.URLStatus{}
	}
	c.JSON(http.StatusOK, statuses)
}

// adminURLMonitorAdd — POST /api/admin/urlmonitor
// Body: { "url": "https://...", "label": "My Site", "interval_seconds": 10 }
func adminURLMonitorAdd(c *gin.Context) {
	var req struct {
		URL             string `json:"url"`
		Label           string `json:"label"`
		IntervalSeconds int    `json:"interval_seconds"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL required"})
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL must start with http:// or https://"})
		return
	}
	if req.IntervalSeconds < 2 {
		req.IntervalSeconds = 2
	}

	doc, err := urlmonitor.AddURL(req.URL, req.Label, req.IntervalSeconds)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "monitor": doc})
}

// adminURLMonitorRemove — DELETE /api/admin/urlmonitor/:id
func adminURLMonitorRemove(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID"})
		return
	}
	if err := urlmonitor.RemoveURL(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// adminURLMonitorHistory — GET /api/admin/urlmonitor/:id/history
func adminURLMonitorHistory(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID"})
		return
	}
	history, err := urlmonitor.GetHistory(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, history)
}

// adminURLMonitorCheck — POST /api/admin/urlmonitor/:id/check
// Triggers one immediate ping and returns the result
func adminURLMonitorCheck(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID"})
		return
	}
	result, err := urlmonitor.ManualCheck(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// ═══════════════════════════════════════════════════════════════════════════════
// MESSAGING — Admin → User email
// POST /api/admin/send-message
// Body: { "user_id": "...", "subject": "...", "message": "..." }
// ═══════════════════════════════════════════════════════════════════════════════

func adminSendMessageToUser(c *gin.Context) {
	var req struct {
		UserID  string `json:"user_id"`
		Subject string `json:"subject"`
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	req.Message = strings.TrimSpace(req.Message)
	if req.UserID == "" || req.Subject == "" || req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "User, subject ani message sagale bharave"})
		return
	}

	userID, err := primitive.ObjectIDFromHex(req.UserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	if user.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Ya user kade email address nahi, message pathavta yet nahi"})
		return
	}

	appName := os.Getenv("BREVO_FROM_NAME")
	if appName == "" {
		appName = "ONLINE COURSE"
	}

	if err := email.SendAdminToUserMessage(user.Email, user.FullName, req.Subject, req.Message, appName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Email pathavta ala nahi: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"username": user.Username,
		"email":    user.Email,
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// BROADCAST — admin announcement + bulk email
// ═══════════════════════════════════════════════════════════════════════════════

// adminBroadcast creates an announcement in DB and sends emails in background goroutine.
// POST /api/admin/broadcast
// Body: { "title": "...", "subject": "...", "message": "...", "target": "all"|"course", "course_id": "..." }
func adminBroadcast(c *gin.Context) {
	var req struct {
		Title    string `json:"title"`
		Subject  string `json:"subject"`
		Message  string `json:"message"`
		Target   string `json:"target"`    // "all" or "course"
		CourseID string `json:"course_id"` // required when target="course"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Subject = strings.TrimSpace(req.Subject)
	req.Message = strings.TrimSpace(req.Message)

	if req.Title == "" || req.Subject == "" || req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title, subject ani message sagale bharave"})
		return
	}
	if req.Target != "all" && req.Target != "course" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target 'all' kinva 'course' pahije"})
		return
	}

	appName := os.Getenv("BREVO_FROM_NAME")
	if appName == "" {
		appName = "ONLINE COURSE"
	}

	// ── Global broadcast ──────────────────────────────────────────────────────
	if req.Target == "all" {
		// Save announcement (CourseID = nil → global popup)
		announcement, err := db.CreateAnnouncement(req.Title, req.Message, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Announcement save zala nahi: " + err.Error()})
			return
		}

		// Fetch all users (quick — just IDs + emails needed)
		users, err := db.ListUsers()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Users load zale nahi: " + err.Error()})
			return
		}

		// Count email-eligible users before launching goroutine
		emailCount := 0
		for _, u := range users {
			if u.Email != "" {
				emailCount++
			}
		}

		// Fire-and-forget: send each email independently — don't wait
		go func(users []db.User, subject, message, title, app string) {
			sent, failed, skipped := 0, 0, 0
			for _, u := range users {
				if u.Email == "" {
					skipped++
					continue
				}
				if err := email.SendAdminToUserMessage(u.Email, u.FullName, subject, message, app); err != nil {
					golog.Printf("[Broadcast/All] Email to %s failed: %v", u.Email, err)
					failed++
				} else {
					sent++
				}
			}
			golog.Printf("[Broadcast/All] '%s' — sent: %d, failed: %d, skipped(no email): %d", title, sent, failed, skipped)
		}(users, req.Subject, req.Message, req.Title, appName)

		c.JSON(http.StatusOK, gin.H{
			"ok":              true,
			"announcement_id": announcement.ID.Hex(),
			"target":          "all",
			"email_queued":    emailCount,
			"message":         "Broadcast suru zala — emails background madhe jaat aahet",
		})
		return
	}

	// ── Course-specific broadcast ─────────────────────────────────────────────
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

	// Save announcement (CourseID set → course-specific banner)
	announcement, err := db.CreateAnnouncement(req.Title, req.Message, &courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Announcement save zala nahi: " + err.Error()})
		return
	}

	// Fetch only users who have access to this course
	users, err := db.GetUsersForCourse(courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Course users load zale nahi: " + err.Error()})
		return
	}

	emailCount := 0
	for _, u := range users {
		if u.Email != "" {
			emailCount++
		}
	}

	// Fire-and-forget per email
	go func(users []db.User, subject, message, title, courseName, app string) {
		sent, failed, skipped := 0, 0, 0
		for _, u := range users {
			if u.Email == "" {
				skipped++
				continue
			}
			if err := email.SendAdminToUserMessage(u.Email, u.FullName, subject, message, app); err != nil {
				golog.Printf("[Broadcast/Course:%s] Email to %s failed: %v", courseName, u.Email, err)
				failed++
			} else {
				sent++
			}
		}
		golog.Printf("[Broadcast/Course:%s] '%s' — sent: %d, failed: %d, skipped: %d", courseName, title, sent, failed, skipped)
	}(users, req.Subject, req.Message, req.Title, course.Title, appName)

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"announcement_id": announcement.ID.Hex(),
		"target":          "course",
		"course":          course.Title,
		"email_queued":    emailCount,
		"message":         "Course broadcast suru zala — emails background madhe jaat aahet",
	})
}

// adminListAnnouncements returns all announcements for admin panel management.
// GET /api/admin/announcements
func adminListAnnouncements(c *gin.Context) {
	list, err := db.ListAllAnnouncements()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// adminDeleteAnnouncement soft-deletes an announcement.
// DELETE /api/admin/announcements/:id
func adminDeleteAnnouncement(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid announcement ID"})
		return
	}
	if err := db.DeleteAnnouncement(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ═══════════════════════════════════════════════════════════════════════════════
// LIVE STREAMS — Admin panel real-time stream tracking
// GET /api/admin/stream/live
// ═══════════════════════════════════════════════════════════════════════════════

type liveStreamView struct {
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email"`
	UserName  string `json:"user_name"`
	FileName  string `json:"file_name"`
	WorkerURL string `json:"worker_url"`
	IsLocal   bool   `json:"is_local"`
	StartedAt string `json:"started_at"`
	StreamKey string `json:"stream_key_hint"` // last 8 chars only — for debug
}

type replacedStreamView struct {
	UserEmail    string `json:"user_email"`
	UserName     string `json:"user_name"`
	OldWorkerURL string `json:"old_worker_url"`
	WasLocal     bool   `json:"was_local"`
	FileName     string `json:"file_name"`
	ReplacedAt   string `json:"replaced_at"`
}

// handleAdminLiveStreams — सध्या चालू असलेल्या सर्व streams ची list देतो.
// active: current live streams (प्रत्येक user एक)
// recently_replaced: 2 min आधी kill झालेले streams (new stream ने replace केले)
func handleAdminLiveStreams(c *gin.Context) {
	// Active streams
	active := liveStreamsList()
	activeOut := make([]liveStreamView, 0, len(active))
	for _, e := range active {
		hint := e.StreamKey
		if len(hint) > 8 {
			hint = "…" + hint[len(hint)-8:]
		}
		activeOut = append(activeOut, liveStreamView{
			UserID:    e.UserID,
			UserEmail: e.UserEmail,
			UserName:  e.UserName,
			FileName:  e.FileName,
			WorkerURL: e.WorkerURL,
			IsLocal:   e.WorkerURL == "",
			StartedAt: e.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			StreamKey: hint,
		})
	}

	// Recently replaced (killed) streams — visible 2 min
	replaced := replacedStreamsList()
	replacedOut := make([]replacedStreamView, 0, len(replaced))
	for _, r := range replaced {
		replacedOut = append(replacedOut, replacedStreamView{
			UserEmail:    r.UserEmail,
			UserName:     r.UserName,
			OldWorkerURL: r.OldWorkerURL,
			WasLocal:     r.OldWorkerURL == "",
			FileName:     r.FileName,
			ReplacedAt:   r.ReplacedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":                true,
		"active":            activeOut,
		"recently_replaced": replacedOut,
		"active_count":      len(activeOut),
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// ADMIN: CHANGE USER PASSWORD
// ═══════════════════════════════════════════════════════════════════════════════

// PUT /api/admin/users/:id/password
// Body: { "password": "newpass123" }
// - Password hash update करतो
// - User ला email notification जातो (email असल्यास)
// - User चा existing session kill होत नाही
func adminChangeUserPassword(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Password) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password required"})
		return
	}
	if len(req.Password) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password must be at least 6 characters"})
		return
	}

	// User fetch — email notification साठी
	user, err := db.GetUserByID(id)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Hash
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash password"})
		return
	}

	// DB update
	if err := db.UpdateUserPassword(id, hash); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "DB update failed: " + err.Error()})
		return
	}

	// Email notification — email असल्यासच, silently fail if email down
	if user.Email != "" {
		go func() {
			if err := email.SendPasswordChangedEmail(user.Email, user.FullName, req.Password); err != nil {
				golog.Printf("[admin] password change mail failed for %s: %v", user.Email, err)
			}
		}()
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "email_sent": user.Email != ""})
}

// ═══════════════════════════════════════════════════════════════════════════════
// ADMIN: IMPERSONATE USER (Login As)
// ═══════════════════════════════════════════════════════════════════════════════

// POST /api/admin/users/:id/impersonate
// - त्या user साठी JWT issue करतो
// - Admin च्या browser मध्ये त्या user चा session cookie set होतो
// - User ला कळत नाही; त्याचा session alive राहतो
func adminImpersonateUser(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	user, err := db.GetUserByID(id)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "User is disabled"})
		return
	}

	// Issue JWT + parallel session (user चा existing session untouched)
	if err := auth.IssueTokenForUser(c, user.ID, user.Username); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "redirect": "/courses"})
}
