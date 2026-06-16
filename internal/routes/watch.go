package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/stream"
	"EverythingSuckz/fsb/internal/token"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	workerreg "EverythingSuckz/fsb/internal/worker"
	"EverythingSuckz/fsb/config"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"io"
	"net/http"
	"strconv"
	"path/filepath"
	"strings"
	"sync"
	"time"

	range_parser "github.com/quantumsheep/range-parser"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
)

// ── Active stream key registry ────────────────────────────────────────────────
var activeStreamKeys sync.Map // userIDHex → streamKey (string)

// nonceStreamKeys — handleWatch madhe ek vela streamKey generate hoto,
// nonce cha key vaparun ithe store hoto.
// handleWatchStream pratyek range request la ithe lookup karto (generate nako).
// Lifetime: StreamTokenTTL (2 hours) — nantarcha auto-delete.
var nonceStreamKeys sync.Map // nonce (string) → streamKey (string)

// ── Admin Panel: Live Stream Tracking ────────────────────────────────────────
type streamMetaEntry struct {
	UserID    string
	UserEmail string
	UserName  string
	FileName  string
	WorkerURL string    // "" = main server local stream
	StreamKey string
	StartedAt time.Time
	ExpiresAt time.Time
}

type replacedStreamEntry struct {
	UserEmail    string
	UserName     string
	OldWorkerURL string
	FileName     string
	ReplacedAt   time.Time
}

var (
	activeStreamMeta     sync.Map // userIDHex → *streamMetaEntry
	recentlyReplacedMeta sync.Map // userIDHex → *replacedStreamEntry (2 min TTL)
)

func storeStreamMeta(userIDHex, userEmail, userName, fileName, workerURL, streamKey string) {
	if existing, ok := activeStreamMeta.Load(userIDHex); ok {
		old := existing.(*streamMetaEntry)
		recentlyReplacedMeta.Store(userIDHex, &replacedStreamEntry{
			UserEmail:    old.UserEmail,
			UserName:     old.UserName,
			OldWorkerURL: old.WorkerURL,
			FileName:     old.FileName,
			ReplacedAt:   time.Now(),
		})
		go func() {
			time.Sleep(2 * time.Minute)
			recentlyReplacedMeta.Delete(userIDHex)
		}()
	}
	activeStreamMeta.Store(userIDHex, &streamMetaEntry{
		UserID:    userIDHex,
		UserEmail: userEmail,
		UserName:  userName,
		FileName:  fileName,
		WorkerURL: workerURL,
		StreamKey: streamKey,
		StartedAt: time.Now(),
		ExpiresAt: time.Now().Add(2 * time.Hour),
	})
}

func liveStreamsList() []*streamMetaEntry {
	now := time.Now()
	var out []*streamMetaEntry
	activeStreamMeta.Range(func(k, v any) bool {
		e := v.(*streamMetaEntry)
		if now.After(e.ExpiresAt) {
			activeStreamMeta.Delete(k)
			return true
		}
		out = append(out, e)
		return true
	})
	return out
}

func replacedStreamsList() []*replacedStreamEntry {
	var out []*replacedStreamEntry
	recentlyReplacedMeta.Range(func(_, v any) bool {
		out = append(out, v.(*replacedStreamEntry))
		return true
	})
	return out
}

func genStreamKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ── Route loader ──────────────────────────────────────────────────────────────

func (e *allRoutes) LoadWatch(r *Route) {
	r.Engine.GET("/watch/:token", auth.RequireAuth, handleWatch)
	r.Engine.GET("/watch-stream/:token", auth.RequireAuth, handleWatchStream)
	r.Engine.GET("/pdf-download/:token", auth.RequireAuth, handlePDFDownload)
	r.Engine.GET("/pdf-view/:token", auth.RequireAuth, handlePDFView)

	// Resume position API
	r.Engine.POST("/api/watch/position/:token", auth.RequireAuth, handleSavePosition)
}

// ── /watch/:token ─────────────────────────────────────────────────────────────
// FIX: Worker token इथेच generate होतो — एकदाच.
// streamURL directly worker URL असतो (redirect नाही).
// त्यामुळे browser seek करताना same worker URL ला Range request पाठवतो —
// worker ला नवीन token येत नाही, नवीन streamKey येत नाही → seek working.

func handleWatch(c *gin.Context) {
	tokenStr := c.Param("token")

	verified, err := token.Verify(tokenStr)
	if err != nil {
		switch err {
		case token.ErrTokenExpired:
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream link has expired. Please go back and reload the course."})
		default:
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Invalid stream link."})
		}
		return
	}

	sess, err := db.GetSessionByToken(verified.SessionToken)
	if err != nil || sess == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Your session has ended. Please log in again to continue watching."})
		return
	}

	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "This stream link belongs to a different account."})
		return
	}

	cachedFile, err := db.GetCachedFile(verified.MessageID, verified.ChannelID)
	if err != nil || cachedFile == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"code": 404, "message": "File not found."})
		return
	}

	hasAccess, err := db.HasAccess(currentUserID, cachedFile.CourseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "You do not have access to this course."})
		return
	}

	// Record view — fire-and-forget
	go func() {
		if err := db.RecordView(currentUserID, verified.MessageID, verified.ChannelID, cachedFile.CourseID); err != nil {
			log.Warn("failed to record view", zap.Error(err))
		}
	}()

	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Server error. Please try again."})
		return
	}
	nonce := hex.EncodeToString(nonceBytes)
	if err := db.UpsertStreamNonce(currentUserID, tokenStr, nonce, time.Now().Add(token.StreamTokenTTL)); err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Server error. Please try again."})
		return
	}

	// StreamKey — एकदाच generate करतो
	watchStreamKey, skErr := genStreamKey()
	if skErr != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Server error. Please try again."})
		return
	}
	nonceStreamKeys.Store(nonce, watchStreamKey)
	activeStreamKeys.Store(currentUserID.Hex(), watchStreamKey)
	go func(n string) {
		time.Sleep(token.StreamTokenTTL + 5*time.Minute)
		nonceStreamKeys.Delete(n)
	}(nonce)

	title := cachedFile.FileName
	if cachedFile.Title != "" {
		title = cachedFile.Title
	}

	mime := strings.ToLower(cachedFile.MimeType)
	mediaType := "video"
	if mime == "application/pdf" {
		mediaType = "pdf"
	} else if strings.HasPrefix(mime, "image/") {
		mediaType = "image"
	} else if strings.HasPrefix(mime, "audio/") {
		mediaType = "audio"
	}

	downloadURL := ""
	viewURL := ""
	if mediaType == "pdf" {
		downloadURL = "/pdf-download/" + tokenStr
		viewURL = "/pdf-view/" + tokenStr
	}

	resumePos, _ := db.GetWatchPosition(currentUserID, verified.MessageID, verified.ChannelID)

	watermark := ""
	if user, err := db.GetUserByID(currentUserID); err == nil && user != nil {
		if user.Email != "" {
			watermark = user.Email
		}
		if user.Mobile != "" {
			if watermark != "" {
				watermark += "  •  " + user.Mobile
			} else {
				watermark = user.Mobile
			}
		}
	}

	// ── Worker routing — इथेच होतो, handleWatchStream मध्ये नाही ──────────
	// Video/audio साठी worker available असेल तर directly worker URL द्यायचा.
	// Browser तोच URL वापरतो seek साठी — नवीन token generate होत नाही.
	// PDF/image साठी worker redirect नको — ते /watch-stream/ वरूनच serve होतात.
	streamURL := "/watch-stream/" + tokenStr + "?n=" + nonce

	if mediaType != "pdf" && mediaType != "image" {
		workerBase := workerreg.Next()
		if workerBase != "" {
			botWorker := bot.GetNextWorker()
			file, fileErr := utils.TimeFuncWithResult(log, "FileFromMessage(handleWatch-worker)", func() (*types.File, error) {
				return utils.FileFromMessageInChannel(c, botWorker.Client, verified.MessageID, cachedFile.ChannelID)
			})
			if fileErr != nil {
				log.Warn("handleWatch: failed to fetch file for worker token — falling back to local stream", zap.Error(fileErr))
			} else {
				workerTok, tokErr := token.NewWorkerToken(
					int64(verified.MessageID), cachedFile.ChannelID, verified.UserID,
					config.ValueOf.ApiID, config.ValueOf.ApiHash, botWorker.BotToken,
					file.FileType,
					file.ID, file.AccessHash, file.FileReference,
					file.ThumbSize,
					file.FileSize, file.FileName, file.MimeType,
					file.DCID,
					watchStreamKey,
					"", // IP check disabled
				)
				if tokErr != nil {
					log.Error("handleWatch: failed to generate worker token — falling back to local stream", zap.Error(tokErr))
				} else {
					// User info for admin tracking
					var liveUserEmail, liveUserName string
					if u, _ := db.GetUserByID(currentUserID); u != nil {
						liveUserEmail = u.Email
						liveUserName = u.FullName
						if liveUserName == "" {
							liveUserName = u.Username
						}
					}
					storeStreamMeta(currentUserID.Hex(), liveUserEmail, liveUserName, cachedFile.FileName, workerBase, watchStreamKey)
					streamURL = workerBase + "/worker-stream/" + workerTok
					log.Debug("handleWatch: using worker stream URL",
						zap.String("worker", workerBase),
						zap.Int("messageID", verified.MessageID),
					)
				}
			}
		}
	}

	setPlayerSecurityHeaders(c)
	c.HTML(http.StatusOK, "player.html", gin.H{
		"title":          title,
		"streamURL":      streamURL,
		"mediaType":      mediaType,
		"downloadURL":    downloadURL,
		"viewURL":        viewURL,
		"resumePosition": resumePos,
		"watchToken":     tokenStr,
		"watermark":      watermark,
	})
}

// ── /watch-stream/:token ──────────────────────────────────────────────────────
// हा आता फक्त LOCAL STREAM करतो.
// Worker available असताना handleWatch directly worker URL देतो —
// त्यामुळे हा endpoint worker साठी कधीच call होत नाही.
// Worker down असेल / fallback असेल तेव्हाच हा वापरला जातो.

func handleWatchStream(c *gin.Context) {
	w := c.Writer
	r := c.Request

	// 1. Token decrypt
	tokenStr := c.Param("token")
	verified, err := token.Verify(tokenStr)
	if err != nil {
		switch err {
		case token.ErrTokenExpired:
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream link has expired. Please go back and reload the course."})
		default:
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Invalid stream link."})
		}
		return
	}

	// 2. Session check
	sess, err := db.GetSessionByToken(verified.SessionToken)
	if err != nil || sess == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Your session has ended. Please log in again to continue watching."})
		return
	}

	// 3. UserID match
	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "This stream link belongs to a different account."})
		return
	}

	// 3b. Nonce check
	nonce := c.Query("n")
	if nonce == "" {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Direct stream access is not allowed. Please use the player."})
		return
	}

	// 3b-i. Range header mandatory
	if r.Header.Get("Range") == "" && r.Method != http.MethodHead {
		if cf, _ := db.GetCachedFile(verified.MessageID, verified.ChannelID); cf == nil || strings.ToLower(cf.MimeType) != "application/pdf" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Range header required for streaming."})
			return
		}
	}

	// 3b-ii. In-memory fast path for seek / buffering range requests.
	_, alreadyActive := nonceStreamKeys.Load(nonce)

	if !alreadyActive {
		// First request — validate from DB.
		streamNonce, nonceErr := db.GetStreamNonce(nonce)
		if nonceErr != nil || streamNonce == nil {
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream session expired or invalid. Please reload the player page."})
			return
		}
		if streamNonce.UserID != currentUserID || streamNonce.WatchToken != tokenStr {
			c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream nonce mismatch. Please reload the player."})
			return
		}
		go db.ConsumeStreamNonce(nonce)
	}

	// 3c. Referer check
	if !requirePlayerReferer(c) {
		c.HTML(http.StatusForbidden, "error.html", gin.H{
			"code":    403,
			"message": "Direct stream access is blocked. Please use the course player.",
		})
		return
	}

	// 3d. StreamKey lookup
	rawSK, skFound := nonceStreamKeys.Load(nonce)
	if !skFound {
		c.HTML(http.StatusForbidden, "error.html", gin.H{
			"code":    403,
			"message": "Stream session expired. Please reload the player page.",
		})
		return
	}
	streamKey := rawSK.(string)

	// 3e. Concurrent stream check
	activeKey, activeExists := activeStreamKeys.Load(currentUserID.Hex())
	if !activeExists || activeKey.(string) != streamKey {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Another stream is already active on your account. Please close other tabs and try again.",
		})
		return
	}

	// 4. File + access check
	cachedFile, err := db.GetCachedFile(verified.MessageID, verified.ChannelID)
	if err != nil || cachedFile == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"code": 404, "message": "File not found."})
		return
	}
	hasAccess, err := db.HasAccess(currentUserID, cachedFile.CourseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "You do not have access to this course."})
		return
	}

	// ── Admin tracking (local stream) ──────────────────────────────────────
	var liveUserEmail, liveUserName string
	if u, _ := db.GetUserByID(currentUserID); u != nil {
		liveUserEmail = u.Email
		liveUserName = u.FullName
		if liveUserName == "" {
			liveUserName = u.Username
		}
	}
	storeStreamMeta(currentUserID.Hex(), liveUserEmail, liveUserName, cachedFile.FileName, "", streamKey)
	log.Debug("watch-stream: local stream (no worker)",
		zap.Int("messageID", verified.MessageID),
	)

	// 5. Local stream
	worker := bot.GetNextWorker()
	file, err := utils.TimeFuncWithResult(log, "FileFromMessage", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(c, worker.Client, verified.MessageID, cachedFile.ChannelID)
	})
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Could not load file from Telegram."})
		return
	}

	// Small file / photo
	if file.FileSize == 0 {
		data, err := downloadPhotoChunked(c, worker.Client.API(), file.Location)
		if err != nil {
			log.Error("photo download failed", zap.Error(err))
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Failed to load photo."})
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))
		c.Header("Content-Length", strconv.Itoa(len(data)))
		setAntiDownloadHeaders(c)
		if r.Method != "HEAD" {
			c.Data(http.StatusOK, file.MimeType, data)
		}
		return
	}

	// Range-aware streaming
	c.Header("Accept-Ranges", "bytes")
	var start, end int64
	rangeHeader := r.Header.Get("Range")

	if rangeHeader == "" {
		start = 0
		end = file.FileSize - 1
		w.WriteHeader(http.StatusOK)
	} else {
		ranges, err := range_parser.Parse(file.FileSize, rangeHeader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		start = ranges[0].Start
		end = ranges[0].End
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize))
		log.Info("watch-stream range", zap.Int64("start", start), zap.Int64("end", end))
		w.WriteHeader(http.StatusPartialContent)
	}

	contentLength := end - start + 1
	mimeType := normalizeWatchMime(file.MimeType, file.FileName)

	c.Header("Content-Type", mimeType)
	c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))
	setAntiDownloadHeaders(c)

	if r.Method == "HEAD" {
		return
	}

	pipe, err := stream.NewStreamPipe(c, worker.Client, file.Location, start, end, log)
	if err != nil {
		log.Error("failed to create stream pipe", zap.Error(err))
		return
	}
	defer pipe.Close()

	// Final streamKey check before serving chunk
	if cur, ok := activeStreamKeys.Load(currentUserID.Hex()); !ok || cur.(string) != streamKey {
		http.Error(w, "Stream replaced by a newer session.", http.StatusTooManyRequests)
		return
	}

	if _, err := io.CopyN(w, pipe, contentLength); err != nil {
		if !utils.IsClientDisconnectError(err) {
			log.Error("stream copy error", zap.Error(err))
		}
	}
}

// ── POST /api/watch/position/:token ──────────────────────────────────────────

func handleSavePosition(c *gin.Context) {
	tokenStr := c.Param("token")

	verified, err := token.Verify(tokenStr)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token"})
		return
	}

	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "token mismatch"})
		return
	}

	var body struct {
		Position float64 `json:"position"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	go func() {
		_ = db.SaveWatchPosition(currentUserID, verified.MessageID, verified.ChannelID, body.Position)
	}()

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func setAntiDownloadHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
}

func setPlayerSecurityHeaders(c *gin.Context) {
	host := config.ValueOf.Host
	mediaSrc := "'self' " + host

	// Env-var workers
	for _, wu := range config.ValueOf.WorkerURLs {
		mediaSrc += " " + wu
	}
	// Admin-added workers
	for _, s := range workerreg.Status() {
		if s["added_by"] == "admin" {
			if u, ok := s["url"].(string); ok && u != "" {
				mediaSrc += " " + u
			}
		}
	}

	connectSrc := "'self'"
	if publicURL := os.Getenv("PUBLIC_URL"); publicURL != "" {
		connectSrc += " " + publicURL
	}

	c.Header("X-Frame-Options", "DENY")
	c.Header("Content-Security-Policy",
		"default-src 'self'; "+
			"media-src "+mediaSrc+"; "+
			"script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src "+connectSrc+"; "+
			"frame-ancestors 'none'")
}

func requirePlayerReferer(c *gin.Context) bool {
	referer := c.Request.Header.Get("Referer")
	if referer == "" {
		return true
	}

	host := config.ValueOf.Host
	if strings.HasPrefix(referer, host+"/watch/") {
		return true
	}

	for _, workerURL := range config.ValueOf.WorkerURLs {
		if strings.HasPrefix(referer, workerURL+"/watch/") {
			return true
		}
	}

	if publicURL := os.Getenv("PUBLIC_URL"); publicURL != "" {
		if strings.HasPrefix(referer, publicURL+"/watch/") {
			return true
		}
	}

	return false
}

// ── /pdf-view/:token ──────────────────────────────────────────────────────────
// ── /pdf-view/:token ──────────────────────────────────────────────────────────

func handlePDFView(c *gin.Context) {
	tokenStr := c.Param("token")

	verified, err := token.Verify(tokenStr)
	if err != nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Invalid or expired link."})
		return
	}

	sess, err := db.GetSessionByToken(verified.SessionToken)
	if err != nil || sess == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Session ended. Please log in again."})
		return
	}

	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "This link belongs to a different account."})
		return
	}

	cachedFile, err := db.GetCachedFile(verified.MessageID, verified.ChannelID)
	if err != nil || cachedFile == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"code": 404, "message": "File not found."})
		return
	}

	if strings.ToLower(cachedFile.MimeType) != "application/pdf" {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Only PDF files can be viewed inline."})
		return
	}

	hasAccess, err := db.HasAccess(currentUserID, cachedFile.CourseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "You do not have access to this course."})
		return
	}

	worker := bot.GetNextWorker()
	file, err := utils.TimeFuncWithResult(log, "FileFromMessage(pdf-view)", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(c, worker.Client, verified.MessageID, cachedFile.ChannelID)
	})
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Could not load file from Telegram."})
		return
	}

	fileName := cachedFile.FileName
	if fileName == "" {
		fileName = "document.pdf"
	}

	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Length", strconv.FormatInt(file.FileSize, 10))
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", fileName))
	c.Header("Cache-Control", "no-store")

	pipe, err := stream.NewStreamPipe(c, worker.Client, file.Location, 0, file.FileSize-1, log)
	if err != nil {
		log.Error("pdf view pipe error", zap.Error(err))
		return
	}
	defer pipe.Close()

	c.Writer.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(c.Writer, pipe, file.FileSize); err != nil {
		if !utils.IsClientDisconnectError(err) {
			log.Error("pdf view copy error", zap.Error(err))
		}
	}
}

// ── /pdf-download/:token ──────────────────────────────────────────────────────

func handlePDFDownload(c *gin.Context) {
	tokenStr := c.Param("token")

	verified, err := token.Verify(tokenStr)
	if err != nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Invalid or expired download link."})
		return
	}

	sess, err := db.GetSessionByToken(verified.SessionToken)
	if err != nil || sess == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Your session has ended. Please log in again."})
		return
	}

	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "This download link belongs to a different account."})
		return
	}

	cachedFile, err := db.GetCachedFile(verified.MessageID, verified.ChannelID)
	if err != nil || cachedFile == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"code": 404, "message": "File not found."})
		return
	}

	if strings.ToLower(cachedFile.MimeType) != "application/pdf" {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Download is only available for PDF files."})
		return
	}

	hasAccess, err := db.HasAccess(currentUserID, cachedFile.CourseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "You do not have access to this course."})
		return
	}

	worker := bot.GetNextWorker()
	file, err := utils.TimeFuncWithResult(log, "FileFromMessage(pdf-download)", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(c, worker.Client, verified.MessageID, cachedFile.ChannelID)
	})
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Could not load file from Telegram."})
		return
	}

	fileName := cachedFile.FileName
	if fileName == "" {
		fileName = "download.pdf"
	}

	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Length", strconv.FormatInt(file.FileSize, 10))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
	c.Header("Cache-Control", "no-store")

	pipe, err := stream.NewStreamPipe(c, worker.Client, file.Location, 0, file.FileSize-1, log)
	if err != nil {
		log.Error("pdf download pipe error", zap.Error(err))
		return
	}
	defer pipe.Close()

	c.Writer.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(c.Writer, pipe, file.FileSize); err != nil {
		if !utils.IsClientDisconnectError(err) {
			log.Error("pdf download copy error", zap.Error(err))
		}
	}
}

// normalizeClientIP — IPv4-mapped IPv6 normalize करतो (logging साठी).
func normalizeClientIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.String()
}

// normalizeWatchMime — MKV, AVI इत्यादी video/mp4 म्हणून serve करतो.
func normalizeWatchMime(mime, fileName string) string {
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/mkv", "video/x-msvideo",
		"video/avi", "video/quicktime":
		return "video/mp4"
	case "video/mp4", "video/webm", "video/ogg":
		return mime
	case "":
		ext := strings.ToLower(filepath.Ext(fileName))
		switch ext {
		case ".webm":
			return "video/webm"
		case ".mp4", ".m4v", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".3gp":
			return "video/mp4"
		case ".mp3":
			return "audio/mpeg"
		case ".pdf":
			return "application/pdf"
		}
		return "video/mp4"
	default:
		if strings.HasPrefix(strings.ToLower(mime), "video/") {
			return "video/mp4"
		}
		return mime
	}
}
