// internal/commands/admin_stream.go
package commands

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// ── Register ──────────────────────────────────────────────────────────────────

func (m *command) LoadAdminStream(d dispatcher.Dispatcher) {
	log := m.log.Named("admin_stream")
	defer log.Sugar().Info("Loaded /adminstream and /userbotdl commands")

	d.AddHandler(handlers.NewCommand("adminstream", adminOnly(cmdAdminStream)))
	d.AddHandler(handlers.NewCommand("userbotdl", adminOnly(cmdUserbotDL)))
}

// ── /adminstream <messageID> ──────────────────────────────────────────────────

func cmdAdminStream(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"📥 *Usage:* `/adminstream <messageID>`\n\n"+
				"LOG_CHANNEL मधील message ID टाका.\n"+
				"Example: `/adminstream 1234`")
		return dispatcher.EndGroups
	}

	msgID, err := strconv.Atoi(a[0])
	if err != nil || msgID < 1 {
		reply(ctx, u, "❌ Valid message ID टाका (positive number).")
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString("⏳ LOG_CHANNEL मधून file आणत आहे..."), nil)

	channelPeer, err := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage)
	if err != nil {
		reply(ctx, u, "❌ LOG_CHANNEL access error: "+err.Error())
		return dispatcher.EndGroups
	}

	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: &tg.InputChannel{
			ChannelID:  channelPeer.ChannelID,
			AccessHash: channelPeer.AccessHash,
		},
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		reply(ctx, u, "❌ Message fetch error: "+err.Error())
		return dispatcher.EndGroups
	}

	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		reply(ctx, u, "❌ Message सापडला नाही (ID: "+strconv.Itoa(msgID)+")")
		return dispatcher.EndGroups
	}

	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		reply(ctx, u, "❌ Message मध्ये media नाही.")
		return dispatcher.EndGroups
	}

	inputMedia, err := buildInputMedia(msg)
	if err != nil {
		reply(ctx, u, "❌ Media error: "+err.Error())
		return dispatcher.EndGroups
	}

	adminChatID := u.EffectiveChat().GetID()
	toPeer := ctx.PeerStorage.GetInputPeerById(adminChatID)

	caption := fmt.Sprintf(
		"📥 Admin Stream\n📌 LOG_CHANNEL msg ID: %d\n🔒 Forward protected",
		msgID,
	)

	_, err = botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       toPeer,
		Media:      inputMedia,
		Message:    caption,
		Noforwards: true,
		RandomID:   rand.Int63(),
	})
	if err != nil {
		reply(ctx, u, "❌ File पाठवताना error: "+err.Error())
		return dispatcher.EndGroups
	}

	if sentMsg != nil {
		_ = ctx.DeleteMessages(adminChatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// ── /userbotdl <chatID> <messageID> ──────────────────────────────────────────

func cmdUserbotDL(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"📥 *Usage:* `/userbotdl <chatID> <messageID>`\n\n"+
				"t.me/c/ वरील positive ID वापरा.\n"+
				"Example: `/userbotdl 2479194095 5613`\n\n"+
				"Telegram link: `t.me/c/2479194095/5613`")
		return dispatcher.EndGroups
	}

	chatIDRaw, err := strconv.ParseInt(a[0], 10, 64)
	if err != nil {
		reply(ctx, u, "❌ Valid chatID टाका.")
		return dispatcher.EndGroups
	}

	var chatID int64
	if chatIDRaw > 0 {
		chatID = -1000000000000 - chatIDRaw
	} else {
		chatID = chatIDRaw
	}

	msgID, err := strconv.Atoi(a[1])
	if err != nil || msgID < 1 {
		reply(ctx, u, "❌ Valid messageID टाका.")
		return dispatcher.EndGroups
	}

	userbotClient := MainUserbotClient
	if userbotClient == nil {
		reply(ctx, u, "❌ Userbot चालू नाही.")
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	// ── Progress message — BOT ने send करतो ───────────────────────────────────
	// IMPORTANT: bot ने send केला म्हणून edit/delete सुद्धा botClient नेच होईल.
	// userbotClient ने edit केल्यास gotd retry loop → CPU 100%
	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(
		fmt.Sprintf("⏳ Channel `%d` | Msg `%d`\n📡 Starting...", chatIDRaw, msgID),
	), nil)

	sentMsgID := 0
	if sentMsg != nil {
		sentMsgID = sentMsg.ID
	}

	adminChatID := u.EffectiveChat().GetID()
	// bot च्या peer storage मधून peer — edit/delete साठी
	botPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)
	// userbot च्या peer storage मधून peer — final send साठी
	userbotPeer := userbotClient.PeerStorage.GetInputPeerById(adminChatID)

	// ── Goroutine — handler ताबडतोब return, site affect नाही ─────────────────
	go func() {
		// 45 minute timeout — Koyeb free tier वर large files साठी पुरेल
		bgCtx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()

		var lastEdit time.Time

		// editMsg: BOT client वापरतो (bot ने message send केला होता)
		editMsg := func(text string) {
			if sentMsgID == 0 {
				return
			}
			if time.Since(lastEdit) < 3*time.Second {
				return
			}
			lastEdit = time.Now()
			editCtx, editCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer editCancel()
			_, _ = botClient.API().MessagesEditMessage(editCtx, &tg.MessagesEditMessageRequest{
				Peer:    botPeer,
				ID:      sentMsgID,
				Message: text,
			})
		}

		// failMsg: throttle नाही — error ताबडतोब दाखवतो
		failMsg := func(text string) {
			if sentMsgID == 0 {
				return
			}
			editCtx, editCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer editCancel()
			_, _ = botClient.API().MessagesEditMessage(editCtx, &tg.MessagesEditMessageRequest{
				Peer:    botPeer,
				ID:      sentMsgID,
				Message: text,
			})
		}

		// ── Step 1: Message fetch ─────────────────────────────────────────────
		editMsg(fmt.Sprintf("⏳ Channel `%d` | Msg `%d`\n📡 Fetching message...", chatIDRaw, msgID))

		tgMsg, err := utils.GetTGMessageInChannel(bgCtx, userbotClient, msgID, chatID)
		if err != nil {
			failMsg(fmt.Sprintf("❌ Message fetch error:\n`%s`", err.Error()))
			return
		}

		// ── Step 2: File metadata ─────────────────────────────────────────────
		file, err := utils.FileFromMessageInChannel(bgCtx, userbotClient, msgID, chatID)
		if err != nil {
			failMsg(fmt.Sprintf("❌ File metadata error:\n`%s`", err.Error()))
			return
		}

		editMsg(fmt.Sprintf(
			"📥 Downloading...\n📄 `%s`\n💾 %s\n░░░░░░░░░░ 0%%",
			file.FileName, formatSize(file.FileSize),
		))

		// ── Step 3: Download (temp fileला) → मग Upload ───────────────────────
		// Sequential: Download 100% झाल्यावरच Upload सुरू होतो
		// Stall detection: 5 मिनिटांत progress नाही → cancel
		stallCtx, stallCancel := context.WithCancel(bgCtx)
		defer stallCancel()
		stallTimer := time.AfterFunc(5*time.Minute, stallCancel)

		uploadedFile, err := downloadAndReupload(stallCtx, userbotClient, file,
			func(written, total int64) {
				stallTimer.Reset(5 * time.Minute)
				if total <= 0 {
					return
				}
				pct := written * 100 / total
				bar := progressBar(int(pct))
				if written >= total {
					// Download 100% — upload stage सुरू होतो
					editMsg(fmt.Sprintf(
						"📤 Uploading to Telegram...\n📄 `%s`\n💾 %s\n%s ✅ Download done",
						file.FileName, formatSize(total), bar,
					))
				} else {
					editMsg(fmt.Sprintf(
						"📥 Downloading...\n📄 `%s`\n💾 %s / %s\n%s %d%%",
						file.FileName,
						formatSize(written), formatSize(total),
						bar, pct,
					))
				}
			},
		)
		stallTimer.Stop()
		if err != nil {
			if stallCtx.Err() != nil {
				failMsg(fmt.Sprintf(
					"❌ Download stalled (5min no progress)\n📄 `%s`\n💡 Retry: `/userbotdl %d %d`",
					file.FileName, chatIDRaw, msgID,
				))
			} else {
				failMsg(fmt.Sprintf("❌ Download/Re-upload error:\n`%s`", err.Error()))
			}
			return
		}

		// ── Step 4: Send ──────────────────────────────────────────────────────
		editMsg(fmt.Sprintf(
			"📤 Sending...\n📄 `%s`\n💾 %s",
			file.FileName, formatSize(file.FileSize),
		))

		inputMedia := &tg.InputMediaUploadedDocument{
			File:     uploadedFile,
			MimeType: file.MimeType,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: file.FileName},
			},
		}

		// LOG_CHANNEL copy (background)
		if markCopied(chatID, msgID) {
			go copyToLogChannelUploaded(tgMsg, inputMedia)
		}

		caption := fmt.Sprintf(
			"📥 Userbot DL\n📌 Chat: %d | Msg: %d\n📄 File: %s\n💾 Size: %s\n🔒 Forward protected",
			chatIDRaw, msgID,
			file.FileName,
			formatSize(file.FileSize),
		)

		sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer sendCancel()
		_, err = userbotClient.API().MessagesSendMedia(sendCtx, &tg.MessagesSendMediaRequest{
			Peer:       userbotPeer,
			Media:      inputMedia,
			Message:    caption,
			Noforwards: true,
			RandomID:   rand.Int63(),
		})
		if err != nil {
			failMsg(fmt.Sprintf("❌ Send error:\n`%s`", err.Error()))
			return
		}

		// ── Step 5: Progress message delete ──────────────────────────────────
		if sentMsgID != 0 {
			delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer delCancel()
			_, _ = botClient.API().MessagesDeleteMessages(delCtx, &tg.MessagesDeleteMessagesRequest{
				Revoke: true,
				ID:     []int{sentMsgID},
			})
		}
	}()

	return dispatcher.EndGroups
}

// ── downloadAndReupload ───────────────────────────────────────────────────────
// Sequential approach: आधी पूर्ण download → temp file → मग upload
// io.Pipe() parallel approach Koyeb free tier वर stall होतो कारण
// slow download → upload pipe block → stall timeout fire होतो.
// Temp file approach: download 100% झाल्यावरच upload सुरू होतो → reliable.

func downloadAndReupload(
	ctx context.Context,
	client *gotgproto.Client,
	file *types.File,
	onProgress func(written, total int64),
) (tg.InputFileClass, error) {

	// ── Step A: Temp file create ──────────────────────────────────────────────
	tmpFile, err := os.CreateTemp("", "userbotdl-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("temp file create failed: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // upload झाल्यावर cleanup

	// ── Step B: Download → temp file ─────────────────────────────────────────
	var w io.Writer = tmpFile
	if onProgress != nil {
		w = &progressWriter{
			w:          tmpFile,
			total:      file.FileSize,
			onProgress: onProgress,
		}
	}

	d := downloader.NewDownloader()
	_, err = d.Download(client.API(), file.Location).Stream(ctx, w)
	tmpFile.Close() // download झाल्यावर close — upload साठी reopen करतो
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// ── Step C: Temp file reopen for upload ───────────────────────────────────
	readFile, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("temp file reopen failed: %w", err)
	}
	defer readFile.Close()

	// ── Step D: Upload ────────────────────────────────────────────────────────
	// WithPartSize(256KB) — Koyeb free tier वर RAM pressure कमी करतो
	u := uploader.NewUploader(client.API()).WithPartSize(256 * 1024)
	uploadedFile, err := u.FromReader(ctx, file.FileName, readFile)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}

	return uploadedFile, nil
}

// ── progressWriter ────────────────────────────────────────────────────────────

type progressWriter struct {
	w          io.Writer
	total      int64
	written    int64
	lastPct    int64
	onProgress func(written, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if p.total > 0 {
		pct := p.written * 100 / p.total
		if pct >= p.lastPct+5 || p.written >= p.total {
			p.lastPct = pct
			if p.onProgress != nil {
				p.onProgress(p.written, p.total)
			}
		}
	}
	return n, err
}

// ── progressBar ──────────────────────────────────────────────────────────────

func progressBar(pct int) string {
	filled := pct / 10
	bar := ""
	for i := 0; i < 10; i++ {
		if i < filled {
			bar += "▓"
		} else {
			bar += "░"
		}
	}
	return bar
}

// ── copyToLogChannel (re-uploaded version) ────────────────────────────────────

var (
	copiedOnce   = make(map[string]struct{})
	copiedOnceMu sync.Mutex
)

func markCopied(chatID int64, messageID int) bool {
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	copiedOnceMu.Lock()
	defer copiedOnceMu.Unlock()
	if _, already := copiedOnce[key]; already {
		return false
	}
	copiedOnce[key] = struct{}{}
	return true
}

func copyToLogChannelUploaded(msg *tg.Message, inputMedia tg.InputMediaClass) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	bgCtx := context.Background()

	logCh, err := utils.GetLogChannelPeer(bgCtx, botClient.API(), botClient.PeerStorage)
	if err != nil {
		utils.Logger.Error("copyToLogChannel: GetLogChannelPeer failed", zap.Error(err))
		return
	}

	toPeer := &tg.InputPeerChannel{
		ChannelID:  logCh.ChannelID,
		AccessHash: logCh.AccessHash,
	}

	_, sendErr := botClient.API().MessagesSendMedia(bgCtx, &tg.MessagesSendMediaRequest{
		Peer:     toPeer,
		Media:    inputMedia,
		Message:  msg.GetMessage(),
		RandomID: rand.Int63(),
		Silent:   true,
	})
	if sendErr != nil {
		utils.Logger.Error("copyToLogChannel: send failed", zap.Error(sendErr))
	} else {
		utils.Logger.Info("copyToLogChannel: copied to log channel")
	}
}

// ── copyToLogChannel (old — non-restricted channels साठी) ────────────────────

func copyToLogChannel(msg *tg.Message, file *types.File) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	bgCtx := context.Background()

	logCh, err := utils.GetLogChannelPeer(bgCtx, botClient.API(), botClient.PeerStorage)
	if err != nil {
		utils.Logger.Error("copyToLogChannel: GetLogChannelPeer failed", zap.Error(err))
		return
	}

	toPeer := &tg.InputPeerChannel{
		ChannelID:  logCh.ChannelID,
		AccessHash: logCh.AccessHash,
	}

	media := buildInputMediaFromFile(file)

	_, sendErr := botClient.API().MessagesSendMedia(bgCtx, &tg.MessagesSendMediaRequest{
		Peer:     toPeer,
		Media:    media,
		Message:  msg.GetMessage(),
		RandomID: rand.Int63(),
		Silent:   true,
	})
	if sendErr != nil {
		utils.Logger.Error("copyToLogChannel: MessagesSendMedia failed",
			zap.Error(sendErr),
			zap.Int64("logChannel", config.ValueOf.LogChannelID),
		)
	} else {
		utils.Logger.Info("copyToLogChannel: file copied to log channel",
			zap.String("fileName", file.FileName),
			zap.Int64("fileSize", file.FileSize),
		)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func buildInputMedia(msg *tg.Message) (tg.InputMediaClass, error) {
	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return nil, fmt.Errorf("invalid document")
		}
		return &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			},
		}, nil

	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil, fmt.Errorf("invalid photo")
		}
		return &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported media type %T", msg.Media)
	}
}

func buildInputMediaFromFile(file *types.File) tg.InputMediaClass {
	if file.FileType == "photo" {
		return &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            file.ID,
				AccessHash:    file.AccessHash,
				FileReference: file.FileReference,
			},
		}
	}
	return &tg.InputMediaDocument{
		ID: &tg.InputDocument{
			ID:            file.ID,
			AccessHash:    file.AccessHash,
			FileReference: file.FileReference,
		},
	}
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1 << 30:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1 << 20:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1 << 10:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

