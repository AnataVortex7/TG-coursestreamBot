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
// प्रत्येक user साठी latest stream चा unique key store करतो.
// नवीन stream सुरू झाला की नवीन key generate होतो — जुना key invalid होतो.
// Worker दर 5 सेकंदाला हा key verify करतो; key मिळाला नाही तर stream बंद करतो.

var activeStreamKeys sync.Map // userIDHex → streamKey (string)

// nonceStreamKeys — handleWatch madhe ek vela streamKey generate hoto,
// nonce cha key vaparun ithe store hoto.
// handleWatchStream pratyek range request la ithe lookup karto (generate nako).
// Lifetime: StreamTokenTTL (2 hours) — nantarcha auto-delete.
var nonceStreamKeys sync.Map // nonce (string) → streamKey (string)

// ── Admin Panel: Live Stream Tracking ────────────────────────────────────────
// प्रत्येक user चा current active stream info admin panel साठी store करतो.
// Main server फक्त redirect करतो त्यामुळे stream end कळत नाही —
// entry 2 तासांनंतर (token TTL) auto-expire होते.
// User ने नवीन stream सुरू केला तर जुनी entry "replaced" म्हणून 2 min दाखवतो.

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

// storeStreamMeta — stream सुरू होताना call करतो.
// जुनी entry असल्यास replaced log मध्ये 2 min साठी save होते.
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

// liveStreamsList — admin panel साठी सर्व active streams देतो (expired clean करतो).
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

// replacedStreamsList — recently replaced (killed) streams देतो (2 min TTL).
func replacedStreamsList() []*replacedStreamEntry {
	var out []*replacedStreamEntry
	recentlyReplacedMeta.Range(func(_, v any) bool {
		out = append(out, v.(*replacedStreamEntry))
		return true
	})
	return out
}

// genStreamKey generates a cryptographically random 16-byte hex key.
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

	// Resume position API
	r.Engine.POST("/api/watch/position/:token", auth.RequireAuth, handleSavePosition)
}

// ── /watch/:token ─────────────────────────────────────────────────────────────

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

	// Record view — fire-and-forget (don't block player load on DB write)
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

	// ── StreamKey — EKDACH generate, nonce sathi store ────────────────────
	// handleWatchStream pratyek range request la hya streamKey la lookup karto.
	// Navya video sathi handleWatch call zhala ki navya streamKey ne
	// activeStreamKeys overwrite hoto — junya video che range requests blocked.
	watchStreamKey, skErr := genStreamKey()
	if skErr != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Server error. Please try again."})
		return
	}
	nonceStreamKeys.Store(nonce, watchStreamKey)
	activeStreamKeys.Store(currentUserID.Hex(), watchStreamKey)
	// TTL nantar auto-cleanup
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
	if mediaType == "pdf" {
		downloadURL = "/pdf-download/" + tokenStr
	}

	// Resume position — server-side load (no extra API call from player)
	resumePos, _ := db.GetWatchPosition(currentUserID, verified.MessageID, verified.ChannelID)

	// Watermark — user चे email + mobile player वर दाखवण्यासाठी
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

	setPlayerSecurityHeaders(c)
	c.HTML(http.StatusOK, "player.html", gin.H{
		"title":          title,
		"streamURL":      "/watch-stream/" + tokenStr + "?n=" + nonce,
		"mediaType":      mediaType,
		"downloadURL":    downloadURL,
		"resumePosition": resumePos,
		"watchToken":     tokenStr,
		"watermark":      watermark,
	})
}

// ── /watch-stream/:token ──────────────────────────────────────────────────────

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
	streamNonce, nonceErr := db.GetStreamNonce(nonce)
	if nonceErr != nil || streamNonce == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream session expired or invalid. Please reload the player page."})
		return
	}
	if streamNonce.UserID != currentUserID || streamNonce.WatchToken != tokenStr {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Stream nonce mismatch. Please reload the player."})
		return
	}

	// 3c. Referer check — फक्त आपल्या player page वरूनच stream होऊ शकेल
	if !requirePlayerReferer(c) {
		c.HTML(http.StatusForbidden, "error.html", gin.H{
			"code":    403,
			"message": "Direct stream access is blocked. Please use the course player.",
		})
		return
	}

	// 3d. StreamKey lookup — handleWatch madhe ek vela generate zhala,
	// ithe faqt lookup karto. Pratyek range request la same key milto.
	// Server restart zhala (nonceStreamKeys clear) tar user la reload sangto.
	rawSK, skFound := nonceStreamKeys.Load(nonce)
	if !skFound {
		c.HTML(http.StatusForbidden, "error.html", gin.H{
			"code":    403,
			"message": "Stream session expired. Please reload the player page.",
		})
		return
	}
	streamKey := rawSK.(string)

	// 3e. Concurrent stream check — he user cha ACTIVE stream ahe ka?
	// Navya video sathi handleWatch call zhala ki activeStreamKeys madhe
	// navya streamKey ne overwrite hoto — ha check tyala block karto.
	activeKey, activeExists := activeStreamKeys.Load(currentUserID.Hex())
	if !activeExists || activeKey.(string) != streamKey {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Another stream is already active on your account. Please close other tabs and try again.",
		})
		return
	}

	// ── User info for admin live stream tracking ─────────────────────────────
	// var ने declare करतो — goto localStream: label च्या आधी असणे आवश्यक आहे
	// (Go मध्ये goto variable declaration cross करत नाही).
	var liveUserEmail, liveUserName string
	if u, _ := db.GetUserByID(currentUserID); u != nil {
		liveUserEmail = u.Email
		liveUserName = u.FullName
		if liveUserName == "" {
			liveUserName = u.Username
		}
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

	// ── 5. Worker routing ──────────────────────────────────────────────────
	workerBase := workerreg.Next()
	if workerBase != "" {
		// Fetch file metadata from Telegram so the worker doesn't have to.
		botWorker := bot.GetNextWorker()
		file, fileErr := utils.TimeFuncWithResult(log, "FileFromMessage(worker)", func() (*types.File, error) {
			return utils.FileFromMessageInChannel(c, botWorker.Client, verified.MessageID, cachedFile.ChannelID)
		})
		if fileErr != nil {
			log.Warn("failed to fetch file for worker token — falling back to local stream", zap.Error(fileErr))
			goto localStream
		}

		workerTok, tokErr := token.NewWorkerToken(
			int64(verified.MessageID), cachedFile.ChannelID, verified.UserID,
			config.ValueOf.ApiID, config.ValueOf.ApiHash, botWorker.BotToken,
			file.FileType,
			file.ID, file.AccessHash, file.FileReference,
			file.ThumbSize,
			file.FileSize, file.FileName, file.MimeType,
			file.DCID,
			streamKey,
		)
		if tokErr != nil {
			log.Error("failed to generate worker token — falling back to local stream", zap.Error(tokErr))
			goto localStream
		}

		workerURL := workerBase + "/worker-stream/" + workerTok
		log.Debug("redirecting to worker",
			zap.String("worker", workerBase),
			zap.Int("messageID", verified.MessageID),
		)
		storeStreamMeta(currentUserID.Hex(), liveUserEmail, liveUserName, cachedFile.FileName, workerBase, streamKey)
		http.Redirect(w, r, workerURL, http.StatusFound)
		return
	}

	// ── 6. Local stream fallback (no healthy workers) ──────────────────────
localStream:
	storeStreamMeta(currentUserID.Hex(), liveUserEmail, liveUserName, cachedFile.FileName, "", streamKey)
	log.Debug("no healthy workers — streaming locally",
		zap.Int("messageID", verified.MessageID),
	)

	worker := bot.GetNextWorker()
	file, err := utils.TimeFuncWithResult(log, "FileFromMessage", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(c, worker.Client, verified.MessageID, cachedFile.ChannelID)
	})
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"code": 500, "message": "Could not load file from Telegram."})
		return
	}

	// Small file / photo — CDN redirect handled
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

	// Local stream — chunk serve karaypurvi ek vela streamKey validate karo.
	// Junya video che range requests ithe block hotat.
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
// JS player दर 10s ला current position save करतो.
// Body: {"position": 123.4}

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

	// Fire-and-forget — player ला wait नको
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

// setPlayerSecurityHeaders — /watch/:token च्या HTML response साठी.
// Player page कुठे embed होणार नाही आणि media फक्त आपल्याच server वरून येईल.
func setPlayerSecurityHeaders(c *gin.Context) {
	host := config.ValueOf.Host
	// Env-var worker URLs + admin-added worker URLs दोन्ही media-src मध्ये हवेत —
	// worker redirect नंतर browser CSP block करतो otherwise.
	mediaSrc := "'self' " + host

	// 1. Env-var workers
	for _, wu := range config.ValueOf.WorkerURLs {
		mediaSrc += " " + wu
	}

	// 2. Admin-added workers (DB मधून registered)
	for _, s := range workerreg.Status() {
		if s["added_by"] == "admin" {
			if u, ok := s["url"].(string); ok && u != "" {
				mediaSrc += " " + u
			}
		}
	}

	c.Header("X-Frame-Options", "DENY")
	c.Header("Content-Security-Policy",
		"default-src 'self'; "+
			"media-src "+mediaSrc+"; "+
			"script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src 'self'; "+
			"frame-ancestors 'none'")
}

// requirePlayerReferer — watch-stream वर येणारी request
// आपल्याच /watch/ page वरून आली आहे का ते check करतो.
// Direct URL access, curl, wget, external downloaders block होतात.
func requirePlayerReferer(c *gin.Context) bool {
	referer := c.Request.Header.Get("Referer")
	if referer == "" {
		// Nonce check आधीच झाला आहे (step 3b) — nonce valid असणे
		// म्हणजे request player page वरूनच आली आहे हे verified आहे.
		// Range requests, worker redirects, काही browsers referer strip करतात —
		// nonce valid असल्यावर referer नसला तरी allow करतो.
		return true
	}
	host := config.ValueOf.Host
	// Referer असल्यास आपल्याच /watch/ page वरून असणे आवश्यक
	return strings.HasPrefix(referer, host+"/watch/")
}

// ── /pdf-download/:token ──────────────────────────────────────────────────────
// Serves a PDF as an attachment (downloadable). Only works for PDF mime type.
// Same auth + token + access checks as watch-stream, but with attachment header.

func handlePDFDownload(c *gin.Context) {
	tokenStr := c.Param("token")

	// 1. Token verify
	verified, err := token.Verify(tokenStr)
	if err != nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Invalid or expired download link."})
		return
	}

	// 2. Session check
	sess, err := db.GetSessionByToken(verified.SessionToken)
	if err != nil || sess == nil {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Your session has ended. Please log in again."})
		return
	}

	// 3. UserID match
	currentUserID := auth.GetUserID(c)
	tokenUserID, err := primitive.ObjectIDFromHex(verified.UserID)
	if err != nil || tokenUserID != currentUserID {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "This download link belongs to a different account."})
		return
	}

	// 4. File lookup
	cachedFile, err := db.GetCachedFile(verified.MessageID, verified.ChannelID)
	if err != nil || cachedFile == nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"code": 404, "message": "File not found."})
		return
	}

	// 5. PDF only
	if strings.ToLower(cachedFile.MimeType) != "application/pdf" {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "Download is only available for PDF files."})
		return
	}

	// 6. Access check
	hasAccess, err := db.HasAccess(currentUserID, cachedFile.CourseID)
	if err != nil || !hasAccess {
		c.HTML(http.StatusForbidden, "error.html", gin.H{"code": 403, "message": "You do not have access to this course."})
		return
	}

	// 7. Fetch file from Telegram and stream as attachment
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
	// attachment = browser downloads instead of opening inline
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

// normalizeWatchMime — stream वेळी browser-safe Content-Type देतो.
// MKV, AVI इत्यादी types video/mp4 म्हणून serve केले जातात.
func normalizeWatchMime(mime, fileName string) string {
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/mkv", "video/x-msvideo",
		"video/avi", "video/quicktime":
		return "video/mp4"
	case "video/mp4", "video/webm", "video/ogg":
		return mime
	case "":
		// mime नाही — extension वरून guess कर
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

