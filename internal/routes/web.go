package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"time"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/token"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Register ─────────────────────────────────────────────────────────────────

func (e *allRoutes) LoadWeb(r *Route) {
	// Login / logout / register — public
	r.Engine.GET("/login", auth.ShowLogin)
	r.Engine.POST("/login", auth.DoLogin)
	r.Engine.GET("/register", auth.ShowRegister)
	r.Engine.POST("/register", auth.DoRegister)
	r.Engine.POST("/logout", auth.RequireAuth, auth.DoLogout)

	// ── Admin Panel ── (static HTML — served directly, no Go logic needed)
	// File: web/admin_panel.html
	// Login is handled inside the HTML itself (JS-only, client-side)
	// Access: http://yourdomain.com/admin
	r.Engine.GET("/admin", func(c *gin.Context) {
		c.HTML(http.StatusOK, "admin_panel.html", gin.H{})
	})

	// Root redirect
	r.Engine.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/courses")
	})

	// Protected pages
	protected := r.Engine.Group("/", auth.RequireAuth)
	{
		protected.GET("/courses", handleCoursesList)
		protected.GET("/course/:id", handleCourseDetail)
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
		ID          string
		Title       string
		Description string
		Thumbnail   string
		Price       string
		BuyLink     string
		FileCount   int64
		ExpiresAt   string // empty = permanent; else "02 Jan 2006"
		IsExpiringSoon bool // true if < 7 days left
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
			// Add expiry info
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

	// Access check
	hasAccess, err := db.HasAccess(userID, courseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{
			"code":    403,
			"message": "You do not have access to this course.",
		})
		return
	}

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
		Index     int
		ID        int
		Title     string
		Caption   string
		FileName  string
		MimeType  string
		FileSize  string
		StreamURL string
		MediaType string
		HasMedia  bool
	}

	playlist := make([]VideoEntry, 0, len(files))
	for i, f := range files {
		streamToken, err := token.New(f.MessageID, f.ChannelID, userID.Hex(), sessionToken)
		if err != nil {
			continue
		}

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

		playlist = append(playlist, VideoEntry{
			Index:     i + 1,
			ID:        f.MessageID,
			Title:     f.Title,
			Caption:   f.Caption,
			FileName:  f.FileName,
			MimeType:  f.MimeType,
			FileSize:  humanSize(f.FileSize),
			StreamURL: "/watch/" + streamToken,
			MediaType: mediaType,
			HasMedia:  true,
		})
	}

	c.HTML(http.StatusOK, "course_detail.html", gin.H{
		"username":   username,
		"course":     course,
		"playlist":   playlist,
		"totalCount": len(playlist),
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
