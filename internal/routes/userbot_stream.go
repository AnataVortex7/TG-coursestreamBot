package routes

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/stream"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync"

	range_parser "github.com/quantumsheep/range-parser"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
)

// ── Duplicate-copy guard ──────────────────────────────────────────────────────
// Ek (chatID, messageID) pair fakt ekdach LOG_CHANNEL la copy hoto.
// Bot restart zhala ki fresh start hoto — that's fine.
var (
	copiedOnce   = make(map[string]struct{})
	copiedOnceMu sync.Mutex
)

func markCopied(chatID int64, messageID int) bool {
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	copiedOnceMu.Lock()
	defer copiedOnceMu.Unlock()
	if _, already := copiedOnce[key]; already {
		return false // already copied
	}
	copiedOnce[key] = struct{}{}
	return true // first time — copy karawa
}

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

	// ── 4. Fetch full tg.Message (file + caption) via userbot ───────────────
	tgMsg, err := utils.GetTGMessageInChannel(ctx, userbotClient, messageID, chatID)
	if err != nil {
		log.Sugar().Errorf("Failed to get tg.Message: %v", err)
		http.Error(w, "could not fetch message: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── 5. Fetch typed file metadata ────────────────────────────────────────
	file, err := utils.TimeFuncWithResult(log, "UserbotFileFromMessage", func() (*types.File, error) {
		return utils.FileFromMessageInChannel(ctx, userbotClient, messageID, chatID)
	})
	if err != nil {
		log.Sugar().Errorf("Failed to get file: %v", err)
		http.Error(w, "could not fetch file: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── 6. Copy file to LOG_CHANNEL (once per chatID+messageID) ─────────────
	// Forward protection ON ahe, tyamule MessagesForwardMessages kaam kart nahi.
	// Mhanun MessagesSendMedia (copy) vaparli — caption preserve hoto.
	if markCopied(chatID, messageID) {
		go copyToLogChannel(tgMsg, file)
	}

	// ── 7. Stream (same logic as /stream/:messageID) ─────────────────────────
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

// copyToLogChannel copies the file (with caption) to LOG_CHANNEL using
// MessagesSendMedia. Forward protection la bypass karto — bot account ne
// nadhi, userbot ne file re-send karto independently.
// Bot client vaparatoy karan tyas LOG_CHANNEL admin/member access aahe.
func copyToLogChannel(msg *tg.Message, file *types.File) {
	botClient := bot.Bot
	if botClient == nil {
		log.Warn("Bot client not available — skipping log channel copy")
		return
	}

	caption := msg.GetMessage() // Telegram caption field

	// Background goroutine mhanun ctx nahi — botClient.API() la context.Background() pathava
	bgCtx := context.Background()

	logCh, err := utils.GetLogChannelPeer(bgCtx, botClient.API(), botClient.PeerStorage)
	if err != nil {
		log.Error("copyToLogChannel: GetLogChannelPeer failed", zap.Error(err))
		return
	}

	toPeer := &tg.InputPeerChannel{
		ChannelID:  logCh.ChannelID,
		AccessHash: logCh.AccessHash,
	}

	// InputDocument build karnya saathi file metadata vapar
	// (userbot ne fetch keleli — forward nahi, direct send)
	var media tg.InputMediaClass
	if file.FileType == "photo" {
		media = &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            file.ID,
				AccessHash:    file.AccessHash,
				FileReference: file.FileReference,
			},
		}
	} else {
		media = &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            file.ID,
				AccessHash:    file.AccessHash,
				FileReference: file.FileReference,
			},
		}
	}

	req := &tg.MessagesSendMediaRequest{
		Peer:     toPeer,
		Media:    media,
		Message:  caption, // original caption
		RandomID: rand.Int63(),
		Silent:   true, // notification nako
	}

	_, sendErr := botClient.API().MessagesSendMedia(bgCtx, req)
	if sendErr != nil {
		log.Error("copyToLogChannel: MessagesSendMedia failed",
			zap.Error(sendErr),
			zap.Int64("logChannel", config.ValueOf.LogChannelID),
		)
	} else {
		log.Info("copyToLogChannel: file copied to log channel",
			zap.String("fileName", file.FileName),
			zap.Int64("fileSize", file.FileSize),
		)
	}
}
