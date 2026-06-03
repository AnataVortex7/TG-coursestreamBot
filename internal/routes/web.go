// internal/routes/web.go
package routes

import (
	"net/http"
	"strings"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/token"
	workerreg "EverythingSuckz/fsb/internal/worker"

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

	// ── OTP login routes ───────────────────────────────────────────────────────

	// GET  /login/check-service  → {"up": true/false}  (Brevo health probe)
	r.Engine.GET("/login/check-service", auth.CheckEmailService)

	// POST /login/otp/send       → send 4-digit OTP to email
	r.Engine.POST("/login/otp/send", auth.SendOTPHandler)

	// POST /login/otp/verify     → verify OTP → set cookie → {"redirect":"/courses"}
	r.Engine.POST("/login/otp/verify", auth.VerifyOTPHandler)

	// POST /login/password       → fallback password login (only when Brevo is down)
	r.Engine.POST("/login/password", auth.DoPasswordLogin)

	// ── Admin Panel ────────────────────────────────────────────────────────────
	// Admin session नसेल तर HTML page देखील serve होत नाही
	// Browser inspect केलं तरी routes/code दिसणार नाही
	r.Engine.GET("/admin", auth.EnsureCSRFCookie, func(c *gin.Context) {
		if !isAdminLoggedIn(c) {
			// Session नाही → login page वर redirect
			c.Redirect(http.StatusFound, "/admin/login")
			return
		}
		c.HTML(http.StatusOK, "admin_panel.html", gin.H{})
	})

	// Admin login page — public (कोणीही access करू शकतो, पण panel नाही)
	r.Engine.GET("/admin/login", func(c *gin.Context) {
		// आधीच logged in असेल तर panel वर redirect
		if isAdminLoggedIn(c) {
			c.Redirect(http.StatusFound, "/admin")
			return
		}
		c.HTML(http.StatusOK, "admin_login.html", gin.H{})
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

		// Announcements API (auth required)
		protected.GET("/api/announcements", handleGlobalAnnouncements)
		protected.GET("/api/announcements/:course_id", handleCourseAnnouncements)
	}
}

// ─── /courses ─────────────────────────────────────────────────────────────────

func handleCoursesList(c *gin.Context) {
	userID := auth.GetUserID(c)
	username := auth.GetUsername(c)

	accessList, err := db.ActiveAccessForUser(userID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load your courses.",
		})
		return
	}

	enrolledMap := make(map[primitive.ObjectID]db.Access, len(accessList))
	for _, a := range accessList {
		enrolledMap[a.CourseID] = a
	}

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
		ExpiresAt      string
		IsExpiringSoon bool
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

// VideoEntry — एक file चा template data
type VideoEntry struct {
	Index        int
	FileKey      string // AES-encrypted {message_id, channel_id} — HTML मध्ये safe
	Title        string
	Caption      string
	FileName     string
	MimeType     string
	FileSize     string
	StreamURL    string
	MediaType    string
	HasMedia     bool
	ViewCount    int
	LastViewedAt string
	CourseIDHex  string
}

// FolderGroup — template ला folder + त्यातले files एकत्र देतो
type FolderGroup struct {
	FolderID   string
	Name       string
	Files      []VideoEntry
	IsUngrouped bool // true = folder नाही, range बाहेरचे files
}

func handleCourseDetail(c *gin.Context) {
	// Wake sleeping admin workers — user is about to watch a video
	go workerreg.WakeAll()

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

	hasAccess, _ := db.HasAccess(userID, courseID)

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{
			"code":    404,
			"message": "Course not found.",
		})
		return
	}

	// सर्व files sort_order नुसार
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load course content.",
		})
		return
	}

	// ── Deduplicate — same video चे .mkv + .mp4 दोन्ही असल्यास एकच दाखवा ──
	// Priority: mp4 > webm > avi > mkv
	formatPriority := func(mime, name string) int {
		mime = strings.ToLower(mime)
		name = strings.ToLower(name)
		if strings.Contains(mime, "mp4") || strings.HasSuffix(name, ".mp4")   { return 4 }
		if strings.Contains(mime, "webm") || strings.HasSuffix(name, ".webm") { return 3 }
		if strings.Contains(mime, "avi") || strings.HasSuffix(name, ".avi")   { return 2 }
		if strings.HasSuffix(name, ".mkv")                                     { return 1 }
		return 0
	}

	// Title normalize करा — extension, resolution tags काढा
	// "[640x360p] .mkv" / "[1280x720p]" / ".mp4" सगळं strip करा
	normalizeTitle := func(title, filename string) string {
		s := strings.TrimSpace(title)
		if s == "" {
			s = strings.TrimSpace(filename)
		}
		s = strings.ToLower(s)
		// extension काढा
		for _, ext := range []string{".mkv", ".mp4", ".avi", ".webm", ".mov", ".flv"} {
			s = strings.ReplaceAll(s, ext, "")
		}
		// resolution tags काढा: [640x360p], [1280x720p], [640x360] etc
		for _, pat := range []string{"[640x360p]", "[1280x720p]", "[640x360]", "[1280x720]", "[1920x1080p]", "[1920x1080]", "[480x360p]", "[480x360]"} {
			s = strings.ReplaceAll(s, pat, "")
		}
		return strings.TrimSpace(s)
	}

	type titleKey = string
	bestFile := make(map[titleKey]db.CachedFile, len(files))
	for _, f := range files {
		key := normalizeTitle(f.Title, f.FileName)
		existing, seen := bestFile[key]
		if !seen {
			bestFile[key] = f
		} else if formatPriority(f.MimeType, f.FileName) > formatPriority(existing.MimeType, existing.FileName) {
			bestFile[key] = f
		}
	}
	// Original order preserve करा
	seenKeys := make(map[titleKey]bool)
	deduped := make([]db.CachedFile, 0, len(bestFile))
	for _, f := range files {
		key := normalizeTitle(f.Title, f.FileName)
		if !seenKeys[key] {
			seenKeys[key] = true
			deduped = append(deduped, bestFile[key])
		}
	}
	files = deduped

	// Folders for this course — order नुसार sorted
	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		folders = []db.CourseFolder{} // error आला तरी page break नाही
	}

	sessionToken := auth.GetSessionToken(c)

	// Watch history — एका DB call मध्ये
	historyRecords, _ := db.GetWatchHistoryForCourse(userID, courseID)
	type viewInfo struct {
		count        int
		lastViewedAt string
	}
	viewMap := make(map[int]viewInfo, len(historyRecords))
	for _, h := range historyRecords {
		viewMap[h.MessageID] = viewInfo{
			count:        h.ViewCount,
			lastViewedAt: h.LastViewedAt.Local().Format("02 Jan 2006, 3:04 PM"),
		}
	}

	// ── Build VideoEntry for each file ────────────────────────────────────────
	buildEntry := func(i int, f db.CachedFile) VideoEntry {
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
			if tok, err := token.New(f.MessageID, f.ChannelID, userID.Hex(), sessionToken); err == nil {
				streamURL = "/watch/" + tok
				hasMedia = true
			}
		}

		fileKey, _ := token.NewFileKey(f.MessageID, f.ChannelID)
		vi := viewMap[f.MessageID]
		return VideoEntry{
			Index:        i + 1,
			FileKey:      fileKey,
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
			CourseIDHex:  courseIDHex,
		}
	}

	// ── Group files into folders ───────────────────────────────────────────────
	// Folders नसल्यास flat playlist (जसं आधी होतं — backward compat)
	if len(folders) == 0 {
		playlist := make([]VideoEntry, 0, len(files))
		for i, f := range files {
			playlist = append(playlist, buildEntry(i, f))
		}
		c.HTML(http.StatusOK, "course_detail.html", gin.H{
			"username":     username,
			"course":       course,
			"playlist":     playlist,      // flat list — जुना template काम करतो
			"folderGroups": []FolderGroup{}, // empty — template folder section skip करतो
			"hasFolders":   false,
			"totalCount":   len(playlist),
			"hasAccess":    hasAccess,
			"botUsername":  config.ValueOf.BotUsername,
		})
		return
	}

	// Folder असल्यास — group करतो
	// map: folderID → FolderGroup (pointer for in-place append)
	groupMap := make(map[primitive.ObjectID]*FolderGroup, len(folders))
	orderedGroups := make([]*FolderGroup, 0, len(folders)+1)

	for _, f := range folders {
		g := &FolderGroup{
			FolderID:    f.ID.Hex(),
			Name:        f.Name,
			Files:       []VideoEntry{},
			IsUngrouped: false,
		}
		groupMap[f.ID] = g
		orderedGroups = append(orderedGroups, g)
	}

	// Ungrouped — range बाहेरचे files
	ungrouped := &FolderGroup{
		FolderID:    "ungrouped",
		Name:        "Ungrouped",
		Files:       []VideoEntry{},
		IsUngrouped: true,
	}

	for i, f := range files {
		entry := buildEntry(i, f)
		matchedFolder := db.FolderForSortOrder(folders, f.SortOrder)
		if matchedFolder == nil {
			ungrouped.Files = append(ungrouped.Files, entry)
		} else {
			groupMap[matchedFolder.ID].Files = append(groupMap[matchedFolder.ID].Files, entry)
		}
	}

	// Final ordered list — empty folders दाखवतो (admin ला visible असतात)
	result := make([]FolderGroup, 0, len(orderedGroups)+1)
	for _, g := range orderedGroups {
		result = append(result, *g)
	}
	// Ungrouped files असतील तरच शेवटी add करतो
	if len(ungrouped.Files) > 0 {
		result = append(result, *ungrouped)
	}

	c.HTML(http.StatusOK, "course_detail.html", gin.H{
		"username":     username,
		"course":       course,
		"playlist":     []VideoEntry{}, // folders असताना हे empty — template folderGroups वापरतो
		"folderGroups": result,
		"hasFolders":   true,
		"totalCount":   len(files),
		"hasAccess":    hasAccess,
		"botUsername":  config.ValueOf.BotUsername,
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

// ─── /api/announcements (global popup) ───────────────────────────────────────

func handleGlobalAnnouncements(c *gin.Context) {
	list, err := db.GetGlobalAnnouncements()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// ─── /api/announcements/:course_id (course banner — access-gated) ─────────────

func handleCourseAnnouncements(c *gin.Context) {
	userID := auth.GetUserID(c)

	courseID, err := primitive.ObjectIDFromHex(c.Param("course_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	hasAccess, err := db.HasAccess(userID, courseID)
	if err != nil || !hasAccess {
		c.JSON(http.StatusOK, []db.Announcement{})
		return
	}

	list, err := db.GetCourseAnnouncements(courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}
