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

	// ── Legal/Policy pages — public ────────────────────────────────────────────
	r.Engine.GET("/terms",   func(c *gin.Context) { c.HTML(http.StatusOK, "terms.html", gin.H{}) })
	r.Engine.GET("/privacy", func(c *gin.Context) { c.HTML(http.StatusOK, "privacy.html", gin.H{}) })
	r.Engine.GET("/refund",  func(c *gin.Context) { c.HTML(http.StatusOK, "refund.html", gin.H{}) })

	// ── Protected pages ────────────────────────────────────────────────────────
	protected := r.Engine.Group("/", auth.RequireAuth)
	{
		protected.GET("/courses", handleCoursesList)
		protected.GET("/course/:id", handleCourseDetail)

		// Lazy-load — एका folder च्या files (large courses साठी fast load)
		protected.GET("/api/course/:course_id/folder/:folder_id/files", handleFolderFiles)

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
		"username":               username,
		"userIDHex":              userID.Hex(),
		"myCourses":              myCourses,
		"available":              available,
		"botUsername":            func() string {
			if config.ValueOf.BotUsername != "" {
				return config.ValueOf.BotUsername
			}
			return "Driven_soulbot"
		}(),
		"telegramContactEnabled": db.GetSettingBool(db.SettingTelegramContact, false),
	})
}

// ─── /course/:id ──────────────────────────────────────────────────────────────

// VideoEntry — एक file चा template data
type VideoEntry struct {
	Index        int    `json:"index"`
	FileKey      string `json:"fileKey"` // AES-encrypted {message_id, channel_id} — HTML मध्ये safe
	Title        string `json:"title"`
	Caption      string `json:"caption"`
	FileName     string `json:"fileName"`
	MimeType     string `json:"mimeType"`
	FileSize     string `json:"fileSize"`
	StreamURL    string `json:"streamURL"`
	MediaType    string `json:"mediaType"`
	HasMedia     bool   `json:"hasMedia"`
	ViewCount    int    `json:"viewCount"`
	LastViewedAt string `json:"lastViewedAt"`
	CourseIDHex  string `json:"courseIDHex"`
}

// FolderGroup — template ला folder + त्यातले files एकत्र देतो
type FolderGroup struct {
	FolderID    string       `json:"folderID"`
	Name        string       `json:"name"`
	Files       []VideoEntry `json:"files"`
	IsUngrouped bool         `json:"isUngrouped"` // true = folder नाही, range बाहेरचे files
	FileCount   int          `json:"fileCount"`   // lazy-load साठी — Files empty असतानाही count दाखवायला
}

// courseContentContext — एका course साठी सगळी loaded data (files, folders, helpers).
// handleCourseDetail आणि handleFolderFiles दोन्ही हे वापरतात — duplicate code टाळण्यासाठी.
type courseContentContext struct {
	userID        primitive.ObjectID
	courseIDHex   string
	hasAccess     bool
	canSeeContent bool // hasAccess || user.IsSpecialUser — fakt list disnyasathi, play/download la nahi
	sessionToken  string
	files         []db.CachedFile // dedup केलेले, sort_order नुसार sorted
	folders       []db.CourseFolder
	viewMap       map[int]struct {
		count        int
		lastViewedAt string
	}
}

// loadCourseContent — course चे files (deduped), folders आणि watch-history एकत्र लोड करतो.
func loadCourseContent(c *gin.Context, courseID primitive.ObjectID) (*courseContentContext, error) {
	userID := auth.GetUserID(c)

	hasAccess, _ := db.HasAccess(userID, courseID)

	// Special user — hasAccess nasel tar pn content list (titles) baghu shakto,
	// pan play/download HasAccess shivay milat nahi (te buildEntry madhe hasAccess
	// var depend aahe, ithe touch kele nahiye).
	canSeeContent := hasAccess
	if !canSeeContent {
		if u, err := db.GetUserByID(userID); err == nil && u != nil && u.IsSpecialUser {
			canSeeContent = true
		}
	}

	// सर्व files sort_order नुसार
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		return nil, err
	}

	// ── Deduplicate — same video चे .mkv + .mp4 दोन्ही असल्यास एकच दाखवा ──
	// Priority: mp4 > webm > avi > mkv
	formatPriority := func(mime, name string) int {
		mime = strings.ToLower(mime)
		name = strings.ToLower(name)
		if strings.Contains(mime, "mp4") || strings.HasSuffix(name, ".mp4") {
			return 4
		}
		if strings.Contains(mime, "webm") || strings.HasSuffix(name, ".webm") {
			return 3
		}
		if strings.Contains(mime, "avi") || strings.HasSuffix(name, ".avi") {
			return 2
		}
		if strings.HasSuffix(name, ".mkv") {
			return 1
		}
		return 0
	}

	// Title normalize करा — extension, resolution tags काढा
	normalizeTitle := func(title, filename string) string {
		s := strings.TrimSpace(title)
		if s == "" {
			s = strings.TrimSpace(filename)
		}
		s = strings.ToLower(s)
		for _, ext := range []string{".mkv", ".mp4", ".avi", ".webm", ".mov", ".flv"} {
			s = strings.ReplaceAll(s, ext, "")
		}
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

	// Folders for this course
	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		folders = []db.CourseFolder{}
	}

	sessionToken := auth.GetSessionToken(c)

	// Watch history
	historyRecords, _ := db.GetWatchHistoryForCourse(userID, courseID)
	viewMap := make(map[int]struct {
		count        int
		lastViewedAt string
	}, len(historyRecords))
	for _, h := range historyRecords {
		viewMap[h.MessageID] = struct {
			count        int
			lastViewedAt string
		}{
			count:        h.ViewCount,
			lastViewedAt: h.LastViewedAt.Local().Format("02 Jan 2006, 3:04 PM"),
		}
	}

	return &courseContentContext{
		userID:        userID,
		courseIDHex:   courseID.Hex(),
		hasAccess:     hasAccess,
		canSeeContent: canSeeContent,
		sessionToken:  sessionToken,
		files:         files,
		folders:       folders,
		viewMap:       viewMap,
	}, nil
}

// buildEntry — एका CachedFile पासून VideoEntry बनवतो (stream token सहित).
func (ctx *courseContentContext) buildEntry(f db.CachedFile) VideoEntry {
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
	if ctx.hasAccess {
		if tok, err := token.New(f.MessageID, f.ChannelID, ctx.userID.Hex(), ctx.sessionToken); err == nil {
			streamURL = "/watch/" + tok
			hasMedia = true
		}
	}

	fileKey, _ := token.NewFileKey(f.MessageID, f.ChannelID)
	vi := ctx.viewMap[f.MessageID]
	return VideoEntry{
		Index:        f.SortOrder,
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
		CourseIDHex:  ctx.courseIDHex,
	}
}

func handleCourseDetail(c *gin.Context) {
	// Wake sleeping admin workers — user is about to watch a video
	go workerreg.WakeAll()

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

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{
			"code":    404,
			"message": "Course not found.",
		})
		return
	}

	ctx, err := loadCourseContent(c, courseID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"code":    500,
			"message": "Failed to load course content.",
		})
		return
	}

	// ── Access nahi ani special pn nahi → fakt Buy Now panel, content list
	// (titles/folders) ajibat pathvat nahi ──────────────────────────────────
	if !ctx.canSeeContent {
		c.HTML(http.StatusOK, "course_detail.html", gin.H{
			"username":      username,
			"course":        course,
			"playlist":      []VideoEntry{},
			"folderGroups":  []FolderGroup{},
			"hasFolders":    false,
			"lazyLoad":      false,
			"totalCount":    0,
			"hasAccess":     false,
			"contentLocked": true,
			"botUsername":   config.ValueOf.BotUsername,
		})
		return
	}

	// ── Folders नसल्यास flat playlist (जसं आधी होतं — backward compat) ────────
	// इथे मात्र performance साठी fix limit ठेवतोय: 200 पेक्षा जास्त files
	// असल्यास सुद्धा "No Folder" नावाचा एकच lazy group बनवतो.
	if len(ctx.folders) == 0 {
		if len(ctx.files) > 200 {
			// Lazy single-group mode — मोठ्या flat courses साठी सुद्धा fast load
			fg := FolderGroup{
				FolderID:    "ungrouped",
				Name:        course.Title,
				Files:       []VideoEntry{},
				IsUngrouped: true,
				FileCount:   len(ctx.files),
			}
			c.HTML(http.StatusOK, "course_detail.html", gin.H{
				"username":      username,
				"course":        course,
				"playlist":      []VideoEntry{},
				"folderGroups":  []FolderGroup{fg},
				"hasFolders":    true,
				"lazyLoad":      true,
				"totalCount":    len(ctx.files),
				"hasAccess":     ctx.hasAccess,
				"contentLocked": false,
				"botUsername":   config.ValueOf.BotUsername,
			})
			return
		}

		playlist := make([]VideoEntry, 0, len(ctx.files))
		for _, f := range ctx.files {
			playlist = append(playlist, ctx.buildEntry(f))
		}
		c.HTML(http.StatusOK, "course_detail.html", gin.H{
			"username":      username,
			"course":        course,
			"playlist":      playlist,
			"folderGroups":  []FolderGroup{},
			"hasFolders":    false,
			"lazyLoad":      false,
			"totalCount":    len(playlist),
			"hasAccess":     ctx.hasAccess,
			"contentLocked": false,
			"botUsername":   config.ValueOf.BotUsername,
		})
		return
	}

	// ── Folders असल्यास — फक्त counts काढतो, files lazy-load होतील ────────────
	counts := make(map[primitive.ObjectID]int, len(ctx.folders))
	ungroupedCount := 0
	for _, f := range ctx.files {
		matchedFolder := db.FolderForSortOrder(ctx.folders, f.SortOrder)
		if matchedFolder == nil {
			ungroupedCount++
		} else {
			counts[matchedFolder.ID]++
		}
	}

	result := make([]FolderGroup, 0, len(ctx.folders)+1)
	for _, f := range ctx.folders {
		result = append(result, FolderGroup{
			FolderID:    f.ID.Hex(),
			Name:        f.Name,
			Files:       []VideoEntry{}, // lazy — page-load वर पाठवत नाही
			IsUngrouped: false,
			FileCount:   counts[f.ID],
		})
	}
	if ungroupedCount > 0 {
		result = append(result, FolderGroup{
			FolderID:    "ungrouped",
			Name:        "Ungrouped",
			Files:       []VideoEntry{},
			IsUngrouped: true,
			FileCount:   ungroupedCount,
		})
	}

	c.HTML(http.StatusOK, "course_detail.html", gin.H{
		"username":      username,
		"course":        course,
		"playlist":      []VideoEntry{},
		"folderGroups":  result,
		"hasFolders":    true,
		"lazyLoad":      true,
		"totalCount":    len(ctx.files),
		"hasAccess":     ctx.hasAccess,
		"contentLocked": false,
		"botUsername":   config.ValueOf.BotUsername,
	})
}

// ─── /api/course/:course_id/folder/:folder_id/files (lazy-load) ───────────────
// folder_id == "ungrouped" → range बाहेरचे files. अन्यथा folder ObjectID hex.
func handleFolderFiles(c *gin.Context) {
	courseIDHex := c.Param("course_id")
	courseID, err := primitive.ObjectIDFromHex(courseIDHex)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course id"})
		return
	}

	folderIDParam := c.Param("folder_id")

	ctx, err := loadCourseContent(c, courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load content"})
		return
	}

	// Access nahi ani special pn nahi → list direct API var pn nahi denar
	if !ctx.canSeeContent {
		c.JSON(http.StatusOK, gin.H{"files": []VideoEntry{}})
		return
	}

	entries := make([]VideoEntry, 0, 64)

	if folderIDParam == "ungrouped" {
		for _, f := range ctx.files {
			if db.FolderForSortOrder(ctx.folders, f.SortOrder) == nil {
				entries = append(entries, ctx.buildEntry(f))
			}
		}
	} else {
		folderID, err := primitive.ObjectIDFromHex(folderIDParam)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid folder id"})
			return
		}
		for _, f := range ctx.files {
			matched := db.FolderForSortOrder(ctx.folders, f.SortOrder)
			if matched != nil && matched.ID == folderID {
				entries = append(entries, ctx.buildEntry(f))
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"files": entries})
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
