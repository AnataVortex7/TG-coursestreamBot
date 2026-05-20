// internal/routes/web.go
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/token"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Register ─────────────────────────────────────────────────────────────────

func (e *allRoutes) LoadWeb(r *Route) {
	// ── Auth — public ──────────────────────────────────────────────────────────

	// Legacy form-based login (kept for backward compat)
	r.Engine.GET("/login", auth.ShowLogin)
	r.Engine.POST("/login", auth.DoLogin)

	// Register
	r.Engine.GET("/register", auth.ShowRegister)
	r.Engine.POST("/register", auth.DoRegister)
	r.Engine.POST("/register/verify", auth.VerifyRegistrationOTP)

	// Logout
	r.Engine.POST("/logout", auth.RequireAuth, auth.DoLogout)

	// ── OTP login routes (NEW) ─────────────────────────────────────────────────

	// GET  /login/check-service  → {"up": true/false}  (Brevo health probe)
	r.Engine.GET("/login/check-service", auth.CheckEmailService)

	// POST /login/otp/send       → send 4-digit OTP to email
	r.Engine.POST("/login/otp/send", auth.SendOTPHandler)

	// POST /login/otp/verify     → verify OTP → set cookie → {"redirect":"/courses"}
	r.Engine.POST("/login/otp/verify", auth.VerifyOTPHandler)

	// POST /login/password       → fallback password login (only when Brevo is down)
	r.Engine.POST("/login/password", auth.DoPasswordLogin)

	// ── Admin Panel ────────────────────────────────────────────────────────────
	r.Engine.GET("/admin", func(c *gin.Context) {
		c.HTML(http.StatusOK, "admin_panel.html", gin.H{})
	})

	// Root redirect
	r.Engine.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/courses")
	})

	// ── Protected pages ────────────────────────────────────────────────────────
	protected := r.Engine.Group("/", auth.RequireAuth)
	{
		protected.GET("/courses", handleCoursesList)
		protected.GET("/course/:id", handleCourseDetail)

		// ── Announcements API (auth required) ──────────────────────────────
		// GET /api/announcements         → global announcements (for popup)
		// GET /api/announcements/:id     → course-specific announcements (access-gated)
		protected.GET("/api/announcements", handleGlobalAnnouncements)
		protected.GET("/api/announcements/:course_id", handleCourseAnnouncements)
	}
}

// ─── /courses ─────────────────────────────────────────────────────────────────

func handleCoursesList(c *gin.Context) {
	userID := auth.GetUserID(c)
	username := auth.GetUsername(c)

	// Access records with expiry info
	accessList, err := db.ActiveAccessForUser(userID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load your courses.",
		})
		return
	}

	// Build map: courseID → access record (for expiry info)
	enrolledMap := make(map[primitive.ObjectID]db.Access, len(accessList))
	for _, a := range accessList {
		enrolledMap[a.CourseID] = a
	}

	// All courses in the system
	allCourses, err := db.ListAllCourses()
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load courses.",
		})
		return
	}

	type CourseCard struct {
		ID             string
		Title          string
		Description    string
		Thumbnail      string
		Price          string
		BuyLink        string
		FileCount      int64
		ExpiresAt      string // empty = permanent; else "02 Jan 2006"
		IsExpiringSoon bool   // true if < 7 days left
	}

	myCourses := make([]CourseCard, 0)
	available := make([]CourseCard, 0)

	for _, course := range allCourses {
		count, _ := db.CountCachedFiles(course.ID)
		card := CourseCard{
			ID:          course.ID.Hex(),
			Title:       course.Title,
			Description: course.Description,
			Thumbnail:   course.Thumbnail,
			Price:       course.Price,
			BuyLink:     course.BuyLink,
			FileCount:   count,
		}
		if acc, ok := enrolledMap[course.ID]; ok {
			if acc.ExpiresAt != nil {
				card.ExpiresAt = acc.ExpiresAt.Format("02 Jan 2006")
				card.IsExpiringSoon = acc.ExpiresAt.Before(time.Now().AddDate(0, 0, 7))
			}
			myCourses = append(myCourses, card)
		} else {
			available = append(available, card)
		}
	}

	c.HTML(http.StatusOK, "courses.html", gin.H{
		"username":  username,
		"myCourses": myCourses,
		"available": available,
	})
}

// ─── /course/:id ──────────────────────────────────────────────────────────────

func handleCourseDetail(c *gin.Context) {
	userID := auth.GetUserID(c)
	username := auth.GetUsername(c)

	courseIDHex := c.Param("id")
	courseID, err := primitive.ObjectIDFromHex(courseIDHex)
	if err != nil {
		c.HTML(http.StatusBadRequest, "error.html", gin.H{
			"code":    400,
			"message": "Invalid course link.",
		})
		return
	}

	// Access check — don't block, just pass flag to template
	hasAccess, _ := db.HasAccess(userID, courseID)

	// Load course info
	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{
			"code":    404,
			"message": "Course not found.",
		})
		return
	}

	// Load all files for this course (sorted by sort_order)
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load course content.",
		})
		return
	}

	// Session token — embedded in stream token for active-session check
	sessionToken := auth.GetSessionToken(c)

	type VideoEntry struct {
		Index        int
		ID           int
		Title        string
		Caption      string
		FileName     string
		MimeType     string
		FileSize     string
		StreamURL    string
		MediaType    string
		HasMedia     bool
		ViewCount    int
		LastViewedAt string // empty if never viewed; else "02 Jan 2006, 3:04 PM"
		ChannelID    int64  // needed for offline token API call
		CourseIDHex  string // needed for offline token API call
	}

	// Load watch history for this course in one DB call (map for O(1) lookup)
	historyRecords, _ := db.GetWatchHistoryForCourse(userID, courseID)
	type viewInfo struct {
		count        int
		lastViewedAt string
	}
	viewMap := make(map[int]viewInfo, len(historyRecords)) // keyed by messageID
	for _, h := range historyRecords {
		viewMap[h.MessageID] = viewInfo{
			count:        h.ViewCount,
			lastViewedAt: h.LastViewedAt.Local().Format("02 Jan 2006, 3:04 PM"),
		}
	}

	playlist := make([]VideoEntry, 0, len(files))
	for i, f := range files {
		mime := strings.ToLower(f.MimeType)
		mediaType := "document"
		switch {
		case strings.Contains(mime, "video"):
			mediaType = "video"
		case strings.Contains(mime, "audio"):
			mediaType = "audio"
		case mime == "application/pdf":
			mediaType = "pdf"
		case strings.Contains(mime, "image"):
			mediaType = "photo"
		}

		streamURL := ""
		hasMedia := false
		if hasAccess {
			streamToken, err := token.New(f.MessageID, f.ChannelID, userID.Hex(), sessionToken)
			if err == nil {
				streamURL = "/watch/" + streamToken
				hasMedia = true
			}
		}

		vi := viewMap[f.MessageID]
		playlist = append(playlist, VideoEntry{
			Index:        i + 1,
			ID:           f.MessageID,
			Title:        f.Title,
			Caption:      f.Caption,
			FileName:     f.FileName,
			MimeType:     f.MimeType,
			FileSize:     humanSize(f.FileSize),
			StreamURL:    streamURL,
			MediaType:    mediaType,
			HasMedia:     hasMedia,
			ViewCount:    vi.count,
			LastViewedAt: vi.lastViewedAt,
			ChannelID:    f.ChannelID,
			CourseIDHex:  courseIDHex,
		})
	}

	c.HTML(http.StatusOK, "course_detail.html", gin.H{
		"username":   username,
		"course":     course,
		"playlist":   playlist,
		"totalCount": len(playlist),
		"hasAccess":  hasAccess,
	})
}

// ─── MIME helpers ─────────────────────────────────────────────────────────────

func isVideo(mime string) bool {
	return len(mime) >= 5 && mime[:5] == "video"
}

func isAudio(mime string) bool {
	return len(mime) >= 5 && mime[:5] == "audio"
}

func isPDF(mime string) bool {
	return mime == "application/pdf"
}

// ─── Size formatter ───────────────────────────────────────────────────────────

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return itoa(int(b)) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB"}
	val := float64(b) / float64(div)
	whole := int(val)
	frac := int((val-float64(whole))*10)
	if frac == 0 {
		return itoa(whole) + " " + suffixes[exp]
	}
	return itoa(whole) + "." + itoa(frac) + " " + suffixes[exp]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}


// ─── /api/announcements  (global popup) ──────────────────────────────────────

func handleGlobalAnnouncements(c *gin.Context) {
	list, err := db.GetGlobalAnnouncements()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// ─── /api/announcements/:course_id  (course banner — access-gated) ───────────

func handleCourseAnnouncements(c *gin.Context) {
	userID := auth.GetUserID(c)

	courseID, err := primitive.ObjectIDFromHex(c.Param("course_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	// Only return announcements to users who have access to the course
	hasAccess, err := db.HasAccess(userID, courseID)
	if err != nil || !hasAccess {
		c.JSON(http.StatusOK, []db.Announcement{}) // empty — no leak of existence
		return
	}

	list, err := db.GetCourseAnnouncements(courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}
