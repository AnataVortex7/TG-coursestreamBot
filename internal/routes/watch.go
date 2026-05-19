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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	range_parser "github.com/quantumsheep/range-parser"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
)

// ── Concurrent stream limiter ─────────────────────────────────────────────────

var streamCounters sync.Map

func acquireStream(userIDHex string) (release func(), ok bool) {
	v, _ := streamCounters.LoadOrStore(userIDHex, new(int32))
	counter := v.(*int32)
	n := atomic.AddInt32(counter, 1)
	if n > 2 {
		atomic.AddInt32(counter, -1)
		return nil, false
	}
	return func() { atomic.AddInt32(counter, -1) }, true
}

// ── Route loader ──────────────────────────────────────────────────────────────

func (e *allRoutes) LoadWatch(r *Route) {
	r.Engine.GET("/watch/:token", auth.RequireAuth, handleWatch)
	r.Engine.GET("/watch-stream/:token", auth.RequireAuth, handleWatchStream)
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

	c.HTML(http.StatusOK, "player.html", gin.H{
		"title":     title,
		"streamURL": "/watch-stream/" + tokenStr + "?n=" + nonce,
		"mediaType": mediaType,
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

	// 3c. Concurrent stream limit
	if r.Method != "HEAD" {
		release, ok := acquireStream(currentUserID.Hex())
		if !ok {
			c.HTML(http.StatusTooManyRequests, "error.html", gin.H{
				"code":    429,
				"message": "Another stream session is already active for your account. Please close other tabs or tools and try again.",
			})
			return
		}
		defer release()
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
		http.Redirect(w, r, workerURL, http.StatusFound)
		return
	}

	// ── 6. Local stream fallback (no healthy workers) ──────────────────────
localStream:
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
	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

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

	if _, err := io.CopyN(w, pipe, contentLength); err != nil {
		if !utils.IsClientDisconnectError(err) {
			log.Error("stream copy error", zap.Error(err))
		}
	}
}

func setAntiDownloadHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
}
