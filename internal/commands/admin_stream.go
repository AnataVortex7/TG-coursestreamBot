// internal/commands/admin_stream.go
//
// Admin-only Telegram bot commands to replace removed HTTP routes:
//
//   /adminstream <messageID>
//       LOG_CHANNEL मधील message ID वरून file bot मध्ये पाठवतो.
//       (पूर्वी: GET /stream/:messageID)
//
//   /userbotdl <chatID> <messageID>
//       Userbot द्वारे कोणत्याही channel मधून file fetch करतो,
//       LOG_CHANNEL मध्ये copy करतो, आणि admin ला पाठवतो.
//       (पूर्वी: GET /dl/:chatID/:messageID)
//
// दोन्ही commands adminOnly wrapper मध्ये आहेत —
// फक्त config.AllowedUsers मध्ये असलेले admin वापरू शकतात.

package commands

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"sync"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
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
// LOG_CHANNEL मधील messageID वरून file admin ला पाठवतो.

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

	// LOG_CHANNEL peer resolve करा
	channelPeer, err := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage)
	if err != nil {
		reply(ctx, u, "❌ LOG_CHANNEL access error: "+err.Error())
		return dispatcher.EndGroups
	}

	// Message fetch करा
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

	// InputMedia build करा
	inputMedia, err := buildInputMedia(msg)
	if err != nil {
		reply(ctx, u, "❌ Media error: "+err.Error())
		return dispatcher.EndGroups
	}

	// Admin ला पाठवा
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

	// Loading message delete करा
	if sentMsg != nil {
		_ = ctx.DeleteMessages(adminChatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// ── /userbotdl <chatID> <messageID> ──────────────────────────────────────────
// Userbot द्वारे कोणत्याही channel मधून file fetch करतो.
// File LOG_CHANNEL मध्ये copy होतो + admin ला पाठवतो.

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

	// t.me/c/ चा positive ID → MTProto negative form
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
		reply(ctx, u, "❌ Userbot चालू नाही. Admin Panel मध्ये TG Login करा किंवा USER_SESSION env set करा.")
		return dispatcher.EndGroups
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(
		fmt.Sprintf("⏳ Channel %d मधून message %d fetch करत आहे...", chatIDRaw, msgID),
	), nil)

	// Userbot द्वारे tg.Message fetch करा (caption साठी)
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

	// LOG_CHANNEL मध्ये copy करा (once per chatID+messageID)
	if markCopied(chatID, msgID) {
		go copyToLogChannel(tgMsg, file)
	}

	// Admin ला file पाठवा (userbot द्वारे)
	adminChatID := u.EffectiveChat().GetID()

	// Admin चा InputPeer userbot च्या storage मधून मिळवा
	adminPeer := userbotClient.PeerStorage.GetInputPeerById(adminChatID)

	caption := fmt.Sprintf(
		"📥 Userbot DL\n📌 Chat: %d | Msg: %d\n📄 File: %s\n💾 Size: %s\n🔒 Forward protected",
		chatIDRaw, msgID,
		file.FileName,
		formatSize(file.FileSize),
	)

	inputMedia := buildInputMediaFromFile(file)

	_, err = userbotClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       adminPeer,
		Media:      inputMedia,
		Message:    caption,
		Noforwards: true,
		RandomID:   rand.Int63(),
	})
	if err != nil {
		// Fallback: bot client द्वारे try करा (LOG_CHANNEL मध्ये copy झाल्यावर)
		reply(ctx, u, "⚠️ Userbot send failed: "+err.Error()+
			"\n\nFile LOG_CHANNEL मध्ये copy झाली असेल — `/adminstream` वापरा.")
		return dispatcher.EndGroups
	}

	// Loading message delete करा
	if sentMsg != nil {
		_ = ctx.DeleteMessages(adminChatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// ── copyToLogChannel ──────────────────────────────────────────────────────────
// Userbot द्वारे fetch केलेली file LOG_CHANNEL ला copy करतो.
// Forward protection bypass: MessagesForwardMessages नाही,
// MessagesSendMedia (independent re-upload) वापरतो.

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

// buildInputMedia — tg.Message मधून InputMediaClass बनवतो.
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

// buildInputMediaFromFile — types.File मधून InputMediaClass बनवतो.
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

// formatSize — bytes ला human-readable string करतो.
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
