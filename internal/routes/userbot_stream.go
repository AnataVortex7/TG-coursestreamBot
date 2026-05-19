package routes

import (
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/stream"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	"fmt"
	"io"
	"net/http"
	"strconv"

	range_parser "github.com/quantumsheep/range-parser"

	"github.com/gin-gonic/gin"
)

// LoadUserbotStream registers the userbot stream route.
// Route: GET /dl/:chatID/:messageID
//
// Example: t.me/c/2479194095/5613  →  /dl/2479194095/5613
//
// Use this when bot is NOT in the group, forward protection is ON,
// but your Telegram account (userbot) IS a member.
// Old route /stream/:messageID is completely untouched.
func (e *allRoutes) LoadUserbotStream(r *Route) {
	defer log.Info("Loaded userbot stream route → /dl/:chatID/:messageID")
	r.Engine.GET("/dl/:chatID/:messageID", userbotStreamHandler)
}

func userbotStreamHandler(ctx *gin.Context) {
	w := ctx.Writer
	r := ctx.Request

	// ── 1. Parse chatID ─────────────────────────────────────────────────────
	// t.me/c/ gives plain positive ID (e.g. 2479194095)
	// MTProto needs full negative form: -1002479194095
	chatIDRaw, err := strconv.ParseInt(ctx.Param("chatID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid chatID: "+err.Error(), http.StatusBadRequest)
		return
	}
	var chatID int64
	if chatIDRaw > 0 {
		chatID = -1000000000000 - chatIDRaw
	} else {
		chatID = chatIDRaw
	}

	// ── 2. Parse messageID ──────────────────────────────────────────────────
	messageID, err := strconv.Atoi(ctx.Param("messageID"))
	if err != nil {
		http.Error(w, "invalid messageID: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── 3. Get userbot client ───────────────────────────────────────────────
	userbotClient := bot.UserBot.GetClient()
	if userbotClient == nil {
		http.Error(w, "userbot not running — set USER_SESSION in env", http.StatusServiceUnavailable)
		return
	}

	// ── 4. Fetch file metadata via userbot ──────────────────────────────────
	file, err := utils.TimeFuncWithResult(log, "UserbotFileFromMessage", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(ctx, userbotClient, messageID, chatID)
	})
	if err != nil {
		log.Sugar().Errorf("Failed to get file: %v", err)
		http.Error(w, "could not fetch file: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── 5. Stream (same logic as /stream/:messageID) ─────────────────────────
	ctx.Header("Accept-Ranges", "bytes")

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
		ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize))
		w.WriteHeader(http.StatusPartialContent)
	}

	contentLength := end - start + 1
	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	ctx.Header("Content-Type", mimeType)
	ctx.Header("Content-Length", strconv.FormatInt(contentLength, 10))

	disposition := "inline"
	if ctx.Query("d") == "true" {
		disposition = "attachment"
	}
	ctx.Header("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"", disposition, file.FileName))

	if r.Method != "HEAD" {
		pipe, err := stream.NewStreamPipe(ctx, userbotClient, file.Location, start, end, log)
		if err != nil {
			log.Sugar().Errorf("Failed to create stream pipe: %v", err)
			return
		}
		defer pipe.Close()
		if _, err := io.CopyN(w, pipe, contentLength); err != nil {
			if !utils.IsClientDisconnectError(err) {
				log.Sugar().Errorf("Error while copying stream: %v", err)
			}
		}
	}
}

