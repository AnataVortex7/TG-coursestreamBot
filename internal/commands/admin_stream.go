// internal/commands/admin_stream.go
package commands

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"sync"

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
// Forward-restricted channels साठी: InputMediaDocument नाही,
// Download → Re-upload → InputMediaUploadedDocument वापरतो.
// त्यामुळे CHAT_FORWARDS_RESTRICTED येत नाही.

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

	// t.me/c/ positive ID → MTProto negative form
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

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(
		fmt.Sprintf("⏳ Channel %d मधून message %d download करत आहे...", chatIDRaw, msgID),
	), nil)

	// Message fetch करा (caption साठी)
	tgMsg, err := utils.GetTGMessageInChannel(ctx, userbotClient, msgID, chatID)
	if err != nil {
		reply(ctx, u, "❌ Message fetch error: "+err.Error())
		return dispatcher.EndGroups
	}

	// File metadata fetch करा
	file, err := utils.FileFromMessageInChannel(ctx, userbotClient, msgID, chatID)
	if err != nil {
		reply(ctx, u, "❌ File metadata error: "+err.Error())
		return dispatcher.EndGroups
	}

	// ── Download + Re-upload (CHAT_FORWARDS_RESTRICTED bypass) ───────────────
	// InputMediaDocument forward check trigger करतो.
	// Download करून नवीन file म्हणून upload केल्यास restriction नाही.
	uploadedFile, err := downloadAndReupload(ctx, userbotClient, file)
	if err != nil {
		reply(ctx, u, "❌ Download/Re-upload error: "+err.Error())
		return dispatcher.EndGroups
	}

	// InputMediaUploadedDocument — नवीन file, forward नाही
	inputMedia := &tg.InputMediaUploadedDocument{
		File:     uploadedFile,
		MimeType: file.MimeType,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: file.FileName},
		},
	}

	// LOG_CHANNEL मध्ये copy करा
	if markCopied(chatID, msgID) {
		go copyToLogChannelUploaded(tgMsg, inputMedia)
	}

	// Admin ला पाठवा
	adminChatID := u.EffectiveChat().GetID()
	adminPeer := userbotClient.PeerStorage.GetInputPeerById(adminChatID)

	caption := fmt.Sprintf(
		"📥 Userbot DL\n📌 Chat: %d | Msg: %d\n📄 File: %s\n💾 Size: %s\n🔒 Forward protected",
		chatIDRaw, msgID,
		file.FileName,
		formatSize(file.FileSize),
	)

	_, err = userbotClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       adminPeer,
		Media:      inputMedia,
		Message:    caption,
		Noforwards: true,
		RandomID:   rand.Int63(),
	})
	if err != nil {
		reply(ctx, u, "❌ Send error: "+err.Error())
		return dispatcher.EndGroups
	}

	if sentMsg != nil {
		_ = ctx.DeleteMessages(adminChatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// ── downloadAndReupload ───────────────────────────────────────────────────────
// File userbot द्वारे download करतो आणि नवीन file म्हणून re-upload करतो.
// हे CHAT_FORWARDS_RESTRICTED bypass करतो कारण InputMediaDocument नाही.

func downloadAndReupload(ctx context.Context, client *gotgproto.Client, file *types.File) (tg.InputFileClass, error) {
	pr, pw := io.Pipe()

	// Download goroutine मध्ये
	dlErrCh := make(chan error, 1)
	go func() {
		defer pw.Close()
		d := downloader.NewDownloader()
		_, err := d.Download(client.API(), file.Location).
			WithSize(file.FileSize).
			DC(file.DCID).
			ToWriter(ctx, pw)
		dlErrCh <- err
	}()

	// Upload (pipe वरून read होतो)
	u := uploader.NewUploader(client.API())
	uploadedFile, upErr := u.FromReader(ctx, file.FileName, pr)

	// Download error check करा
	dlErr := <-dlErrCh
	if dlErr != nil {
		return nil, fmt.Errorf("download failed: %w", dlErr)
	}
	if upErr != nil {
		return nil, fmt.Errorf("upload failed: %w", upErr)
	}

	return uploadedFile, nil
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

// ── copyToLogChannel (old — LOG_CHANNEL साठी, non-restricted) ────────────────

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
