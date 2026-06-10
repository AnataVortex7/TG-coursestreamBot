// internal/commands/admin_stream.go
package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"path/filepath"
	"strings"
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

// ── Caption Replace Store ─────────────────────────────────────────────────────
var captionReplaceStore sync.Map // map[string]string

// ── Send Channel Store ────────────────────────────────────────────────────────
var sendChannelStore sync.Map // key: "id", value: int64

func getSendChannelID() (int64, bool) {
	v, ok := sendChannelStore.Load("id")
	if !ok {
		return 0, false
	}
	return v.(int64), true
}

// ── Thumbnail Store ───────────────────────────────────────────────────────────
var thumbnailStore sync.Map // key: "thumb", value: []byte

func getStoredThumbnailBytes() ([]byte, bool) {
	v, ok := thumbnailStore.Load("thumb")
	if !ok {
		return nil, false
	}
	return v.([]byte), true
}

func hasThumbnailStored() bool {
	_, ok := thumbnailStore.Load("thumb")
	return ok
}

// ── Watermark Store ───────────────────────────────────────────────────────────
// /setwatermark ने set केलेला text — video वर bottom-right corner मध्ये overlay
// Server restart वर reset होतो (in-memory only)

var watermarkStore sync.Map // key: "text", value: string

func getWatermarkText() (string, bool) {
	v, ok := watermarkStore.Load("text")
	if !ok {
		return "", false
	}
	return v.(string), true
}

func hasWatermark() bool {
	_, ok := watermarkStore.Load("text")
	return ok
}

// ── Stop Store ────────────────────────────────────────────────────────────────
// चालू असलेल्या /userbotdl process ला cancel करायला
// key: "cancel", value: context.CancelFunc

var activeProcessStore sync.Map // key: "cancel", value: context.CancelFunc

func setActiveCancel(cancel context.CancelFunc) {
	activeProcessStore.Store("cancel", cancel)
}

func clearActiveCancel() {
	activeProcessStore.Delete("cancel")
}

func stopActiveProcess() bool {
	v, ok := activeProcessStore.Load("cancel")
	if !ok {
		return false
	}
	cancel := v.(context.CancelFunc)
	cancel()
	activeProcessStore.Delete("cancel")
	return true
}

func hasActiveProcess() bool {
	_, ok := activeProcessStore.Load("cancel")
	return ok
}

// ── Register ──────────────────────────────────────────────────────────────────

func (m *command) LoadAdminStream(d dispatcher.Dispatcher) {
	log := m.log.Named("admin_stream")
	defer log.Sugar().Info("Loaded /adminstream /userbotdl /stopuserbotdl /setreplace /listreplace /delreplace /clearreplace /setsendchannel /showsendchannel /clearsendchannel /setthumbnail /clearthumbnail /showthumbnail /setwatermark /clearwatermark /showwatermark commands")

	d.AddHandler(handlers.NewCommand("adminstream", adminOnly(cmdAdminStream)))
	d.AddHandler(handlers.NewCommand("userbotdl", adminOnly(cmdUserbotDL)))
	d.AddHandler(handlers.NewCommand("stopuserbotdl", adminOnly(cmdStopUserbotDL)))
	d.AddHandler(handlers.NewCommand("setreplace", adminOnly(cmdSetReplace)))
	d.AddHandler(handlers.NewCommand("listreplace", adminOnly(cmdListReplace)))
	d.AddHandler(handlers.NewCommand("delreplace", adminOnly(cmdDelReplace)))
	d.AddHandler(handlers.NewCommand("clearreplace", adminOnly(cmdClearReplace)))
	d.AddHandler(handlers.NewCommand("setsendchannel", adminOnly(cmdSetSendChannel)))
	d.AddHandler(handlers.NewCommand("showsendchannel", adminOnly(cmdShowSendChannel)))
	d.AddHandler(handlers.NewCommand("clearsendchannel", adminOnly(cmdClearSendChannel)))
	d.AddHandler(handlers.NewCommand("setthumbnail", adminOnly(cmdSetThumbnail)))
	d.AddHandler(handlers.NewCommand("clearthumbnail", adminOnly(cmdClearThumbnail)))
	d.AddHandler(handlers.NewCommand("showthumbnail", adminOnly(cmdShowThumbnail)))
	d.AddHandler(handlers.NewCommand("setwatermark", adminOnly(cmdSetWatermark)))
	d.AddHandler(handlers.NewCommand("clearwatermark", adminOnly(cmdClearWatermark)))
	d.AddHandler(handlers.NewCommand("showwatermark", adminOnly(cmdShowWatermark)))
}

// ── /stopuserbotdl ────────────────────────────────────────────────────────────
// सध्या चालू असलेला /userbotdl process cancel करतो.
// फक्त current running process थांबवतो — complete झालेले messages undo होत नाहीत.

func cmdStopUserbotDL(ctx *ext.Context, u *ext.Update) error {
	if !hasActiveProcess() {
		reply(ctx, u, "⚠️ सध्या कोणताही `/userbotdl` process चालू नाही.")
		return dispatcher.EndGroups
	}
	stopped := stopActiveProcess()
	if stopped {
		reply(ctx, u, "🛑 `/userbotdl` process stop केला!\n\nजे messages आधीच send झाले ते cancel होणार नाहीत.\nSummary लवकरच update होईल.")
	} else {
		reply(ctx, u, "⚠️ Process आधीच थांबला होता.")
	}
	return dispatcher.EndGroups
}

// ── /setwatermark <text> ──────────────────────────────────────────────────────
// Video uploads वर bottom-right corner मध्ये text watermark overlay करतो.
// Usage: /setwatermark @yourchannel

func cmdSetWatermark(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		cur, ok := getWatermarkText()
		if ok {
			reply(ctx, u, fmt.Sprintf(
				"💧 *Watermark*\n\nसध्या set: `%s`\n\nबदलायला: `/setwatermark <text>`\nकाढायला: /clearwatermark",
				cur,
			))
		} else {
			reply(ctx, u,
				"💧 *Usage:* `/setwatermark <text>`\n\n"+
					"Example: `/setwatermark @yourchannel`\n\n"+
					"Video uploads वर bottom-right corner मध्ये हा text दिसेल.\n"+
					"⚠️ FFmpeg लागतो — processing time जास्त असू शकतो.")
		}
		return dispatcher.EndGroups
	}

	text := strings.Join(a, " ")
	watermarkStore.Store("text", text)
	reply(ctx, u, fmt.Sprintf(
		"✅ Watermark set!\n\n💧 Text: `%s`\n\nआता सगळ्या `/userbotdl` video uploads वर हा watermark लागेल.\nकाढायला: /clearwatermark",
		text,
	))
	return dispatcher.EndGroups
}

// ── /clearwatermark ───────────────────────────────────────────────────────────

func cmdClearWatermark(ctx *ext.Context, u *ext.Update) error {
	if !hasWatermark() {
		reply(ctx, u, "💧 कोणताही watermark set नाही.")
		return dispatcher.EndGroups
	}
	watermarkStore.Delete("text")
	reply(ctx, u, "✅ Watermark clear केला. Video uploads आता watermark शिवाय जातील.")
	return dispatcher.EndGroups
}

// ── /showwatermark ────────────────────────────────────────────────────────────

func cmdShowWatermark(ctx *ext.Context, u *ext.Update) error {
	text, ok := getWatermarkText()
	if !ok {
		reply(ctx, u, "💧 कोणताही watermark set नाही.\nSet करायला: `/setwatermark <text>`")
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf(
		"💧 *Watermark*\n\nText: `%s`\nPosition: Bottom-right corner\n\nकाढायला: /clearwatermark",
		text,
	))
	return dispatcher.EndGroups
}

// ── /setreplace <old=new> [old2=new2] ... ─────────────────────────────────────

func cmdSetReplace(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"📝 *Usage:* `/setreplace old=new [old2=new2 ...]`\n\n"+
				"Examples:\n"+
				"`/setreplace hello=नमस्ते`\n"+
				"`/setreplace Course=कोर्स Bot=बॉट`\n\n"+
				"Caption मधील 'old' word आढळल्यास 'new' ने replace होईल.\n"+
				"एकावेळी multiple pairs देता येतात.\n"+
				"सध्याचे rules बघायला: /listreplace")
		return dispatcher.EndGroups
	}

	added := []string{}
	invalid := []string{}

	for _, pair := range a {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			invalid = append(invalid, pair)
			continue
		}
		oldWord := strings.TrimSpace(parts[0])
		newWord := strings.TrimSpace(parts[1])
		captionReplaceStore.Store(oldWord, newWord)
		added = append(added, fmt.Sprintf("`%s` → `%s`", oldWord, newWord))
	}

	msg := ""
	if len(added) > 0 {
		msg += fmt.Sprintf("✅ Replace rules set (%d):\n%s", len(added), strings.Join(added, "\n"))
	}
	if len(invalid) > 0 {
		msg += fmt.Sprintf("\n\n❌ Invalid pairs (= नाही):\n%s", strings.Join(invalid, ", "))
	}
	if msg == "" {
		msg = "❌ कोणताही valid pair नाही."
	}
	reply(ctx, u, msg)
	return dispatcher.EndGroups
}

// ── /listreplace ──────────────────────────────────────────────────────────────

func cmdListReplace(ctx *ext.Context, u *ext.Update) error {
	var pairs []string
	captionReplaceStore.Range(func(k, v interface{}) bool {
		pairs = append(pairs, fmt.Sprintf("`%s` → `%s`", k.(string), v.(string)))
		return true
	})
	if len(pairs) == 0 {
		reply(ctx, u, "📋 कोणतेही replace rules नाहीत.\nAdd करायला: `/setreplace old=new`")
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("📋 Caption Replace Rules (%d):\n\n%s", len(pairs), strings.Join(pairs, "\n")))
	return dispatcher.EndGroups
}

// ── /delreplace <old> ─────────────────────────────────────────────────────────

func cmdDelReplace(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: `/delreplace <old_word>`\nExample: `/delreplace hello`")
		return dispatcher.EndGroups
	}
	oldWord := strings.TrimSpace(a[0])
	if _, ok := captionReplaceStore.Load(oldWord); !ok {
		reply(ctx, u, fmt.Sprintf("❌ Rule नाही: `%s`\nसध्याचे rules: /listreplace", oldWord))
		return dispatcher.EndGroups
	}
	captionReplaceStore.Delete(oldWord)
	reply(ctx, u, fmt.Sprintf("✅ Deleted: `%s`", oldWord))
	return dispatcher.EndGroups
}

// ── /clearreplace ─────────────────────────────────────────────────────────────

func cmdClearReplace(ctx *ext.Context, u *ext.Update) error {
	count := 0
	captionReplaceStore.Range(func(k, v interface{}) bool {
		captionReplaceStore.Delete(k)
		count++
		return true
	})
	reply(ctx, u, fmt.Sprintf("✅ सगळे replace rules clear केले (%d rules काढले).", count))
	return dispatcher.EndGroups
}

// ── /setsendchannel <channelID> ──────────────────────────────────────────────

func cmdSetSendChannel(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		curID, ok := getSendChannelID()
		if ok {
			displayID := -(curID + 1000000000000)
			reply(ctx, u, fmt.Sprintf(
				"📢 *Send Channel*\n\nSध्या set: `%d`\n\nबदलायला: `/setsendchannel <channelID>`\nकाढायला: /clearsendchannel",
				displayID,
			))
		} else {
			reply(ctx, u,
				"📢 *Usage:* `/setsendchannel <channelID>`\n\n"+
					"t.me/c/ वरील positive channel ID टाका.\n"+
					"Example: `/setsendchannel 2479194095`\n\n"+
					"⚠️ Bot त्या channel चा owner किंवा admin असणे आवश्यक आहे.\n"+
					"/userbotdl चे सगळे files + text msgs तिथे पण send होतील.")
		}
		return dispatcher.EndGroups
	}

	rawID, err := strconv.ParseInt(strings.TrimSpace(a[0]), 10, 64)
	if err != nil || rawID <= 0 {
		reply(ctx, u, "❌ Invalid channel ID. Positive number टाका.\nExample: `2479194095`")
		return dispatcher.EndGroups
	}

	internalID := -1000000000000 - rawID

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	peer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, internalID)
	if err != nil {
		reply(ctx, u, fmt.Sprintf(
			"❌ Channel access नाही (ID: `%d`).\n\nCheck करा:\n• Bot त्या channel मध्ये admin/owner आहे का?\n• Channel ID बरोबर आहे का?\n\nError: `%s`",
			rawID, err.Error(),
		))
		return dispatcher.EndGroups
	}
	_ = peer

	sendChannelStore.Store("id", internalID)

	reply(ctx, u, fmt.Sprintf(
		"✅ Send channel set!\n\n📢 Channel ID: `%d`\n\n"+
			"/userbotdl चे सगळे files + text msgs आता या channel ला पण send होतील.\n"+
			"काढायला: /clearsendchannel",
		rawID,
	))
	return dispatcher.EndGroups
}

// ── /showsendchannel ──────────────────────────────────────────────────────────

func cmdShowSendChannel(ctx *ext.Context, u *ext.Update) error {
	curID, ok := getSendChannelID()
	if !ok {
		reply(ctx, u, "📢 कोणताही send channel set नाही.\nSet करायला: `/setsendchannel <channelID>`")
		return dispatcher.EndGroups
	}
	displayID := -(curID + 1000000000000)
	reply(ctx, u, fmt.Sprintf(
		"📢 *Send Channel*\n\nChannel ID: `%d`\n\nकाढायला: /clearsendchannel",
		displayID,
	))
	return dispatcher.EndGroups
}

// ── /clearsendchannel ─────────────────────────────────────────────────────────

func cmdClearSendChannel(ctx *ext.Context, u *ext.Update) error {
	_, ok := getSendChannelID()
	if !ok {
		reply(ctx, u, "📢 आधीच कोणताही send channel set नाही.")
		return dispatcher.EndGroups
	}
	sendChannelStore.Delete("id")
	reply(ctx, u, "✅ Send channel clear केला. आता फक्त admin chat मध्ये send होईल.")
	return dispatcher.EndGroups
}

// ── /setthumbnail ────────────────────────────────────────────────────────────
// [FIX] Channel peer fetch robust केला — type switch improve केला

func cmdSetThumbnail(ctx *ext.Context, u *ext.Update) error {
	tgMsg := u.EffectiveMessage.Message
	if tgMsg == nil {
		reply(ctx, u, "❌ Message मिळाला नाही.")
		return dispatcher.EndGroups
	}

	replyHeader, ok := tgMsg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || replyHeader == nil {
		reply(ctx, u, "📸 *Usage:* एखाद्या photo ला reply करून `/setthumbnail` पाठवा.\n\nतो photo सगळ्या `/userbotdl` video uploads चा thumbnail होईल.")
		return dispatcher.EndGroups
	}

	replyMsgID := replyHeader.ReplyToMsgID

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	chatID := u.EffectiveChat().GetID()
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fetchCancel()

	// [FIX] Chat type detect करून correct API call वापरा
	// GetInputPeerById चा result check करा — channel असेल तर ChannelsGetMessages
	var replyMsg *tg.Message

	inputPeer := botClient.PeerStorage.GetInputPeerById(chatID)
	switch peer := inputPeer.(type) {
	case *tg.InputPeerChannel:
		// Channel / supergroup — ChannelsGetMessages वापरा
		res, err := botClient.API().ChannelsGetMessages(fetchCtx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: replyMsgID}},
		})
		if err != nil {
			reply(ctx, u, fmt.Sprintf("❌ Reply message fetch failed: `%s`", err.Error()))
			return dispatcher.EndGroups
		}
		msgs, ok := res.(*tg.MessagesChannelMessages)
		if !ok || len(msgs.Messages) == 0 {
			reply(ctx, u, "❌ Reply message सापडला नाही.")
			return dispatcher.EndGroups
		}
		m, ok := msgs.Messages[0].(*tg.Message)
		if !ok {
			reply(ctx, u, "❌ Reply message invalid आहे.")
			return dispatcher.EndGroups
		}
		replyMsg = m

	case *tg.InputPeerChat:
		// Group chat — MessagesGetMessages वापरा
		res, err := botClient.API().MessagesGetMessages(fetchCtx, []tg.InputMessageClass{
			&tg.InputMessageID{ID: replyMsgID},
		})
		if err != nil {
			reply(ctx, u, fmt.Sprintf("❌ Reply message fetch failed: `%s`", err.Error()))
			return dispatcher.EndGroups
		}
		msgSlice, ok := res.(*tg.MessagesMessages)
		if !ok || len(msgSlice.Messages) == 0 {
			reply(ctx, u, "❌ Reply message सापडला नाही.")
			return dispatcher.EndGroups
		}
		m, ok := msgSlice.Messages[0].(*tg.Message)
		if !ok {
			reply(ctx, u, "❌ Reply message invalid आहे.")
			return dispatcher.EndGroups
		}
		replyMsg = m

	case *tg.InputPeerUser:
		// Private chat (DM)
		res, err := botClient.API().MessagesGetMessages(fetchCtx, []tg.InputMessageClass{
			&tg.InputMessageID{ID: replyMsgID},
		})
		if err != nil {
			reply(ctx, u, fmt.Sprintf("❌ Reply message fetch failed: `%s`", err.Error()))
			return dispatcher.EndGroups
		}
		// Private chat मध्ये MessagesMessages किंवा MessagesSlice येतो
		var msgs []tg.MessageClass
		switch r := res.(type) {
		case *tg.MessagesMessages:
			msgs = r.Messages
		case *tg.MessagesMessagesSlice:
			msgs = r.Messages
		default:
			reply(ctx, u, "❌ Reply message response type unknown.")
			return dispatcher.EndGroups
		}
		if len(msgs) == 0 {
			reply(ctx, u, "❌ Reply message सापडला नाही.")
			return dispatcher.EndGroups
		}
		m, ok := msgs[0].(*tg.Message)
		if !ok {
			reply(ctx, u, "❌ Reply message invalid आहे.")
			return dispatcher.EndGroups
		}
		replyMsg = m

	default:
		// Fallback — unknown peer type
		reply(ctx, u, fmt.Sprintf("❌ Chat peer type unknown: `%T`\nBot ला या chat मध्ये access नाही का?", inputPeer))
		return dispatcher.EndGroups
	}

	// Photo check
	photo, ok := replyMsg.Media.(*tg.MessageMediaPhoto)
	if !ok {
		reply(ctx, u, "❌ Reply केलेला message photo नाही.\nफक्त photo support आहे.")
		return dispatcher.EndGroups
	}
	photoObj, ok := photo.Photo.(*tg.Photo)
	if !ok {
		reply(ctx, u, "❌ Photo object invalid आहे.")
		return dispatcher.EndGroups
	}

	// Biggest size निवडा
	var biggestSize *tg.PhotoSize
	for _, sz := range photoObj.Sizes {
		if ps, ok := sz.(*tg.PhotoSize); ok {
			if biggestSize == nil || ps.W > biggestSize.W {
				biggestSize = ps
			}
		}
	}
	if biggestSize == nil {
		reply(ctx, u, "❌ Photo size मिळाला नाही.")
		return dispatcher.EndGroups
	}

	loc := &tg.InputPhotoFileLocation{
		ID:            photoObj.ID,
		AccessHash:    photoObj.AccessHash,
		FileReference: photoObj.FileReference,
		ThumbSize:     biggestSize.Type,
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer upCancel()

	tmpFile, err := os.CreateTemp("", "thumb-*.jpg")
	if err != nil {
		reply(ctx, u, fmt.Sprintf("❌ Temp file create failed: `%s`", err.Error()))
		return dispatcher.EndGroups
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	d := downloader.NewDownloader()
	_, err = d.Download(botClient.API(), loc).Stream(upCtx, tmpFile)
	tmpFile.Close()
	if err != nil {
		reply(ctx, u, fmt.Sprintf("❌ Photo download failed: `%s`", err.Error()))
		return dispatcher.EndGroups
	}

	readFile, err := os.Open(tmpPath)
	if err != nil {
		reply(ctx, u, fmt.Sprintf("❌ File open failed: `%s`", err.Error()))
		return dispatcher.EndGroups
	}
	defer readFile.Close()

	thumbBytes, err := io.ReadAll(readFile)
	if err != nil {
		reply(ctx, u, fmt.Sprintf("❌ Thumbnail read failed: `%s`", err.Error()))
		return dispatcher.EndGroups
	}
	if len(thumbBytes) == 0 {
		reply(ctx, u, "❌ Thumbnail file empty आहे.")
		return dispatcher.EndGroups
	}

	thumbnailStore.Store("thumb", thumbBytes)
	reply(ctx, u, "✅ Thumbnail set!\n\nआता सगळ्या `/userbotdl` video uploads ला हा photo thumbnail म्हणून लागेल.\nकाढायला: /clearthumbnail")
	return dispatcher.EndGroups
}

// ── /clearthumbnail ───────────────────────────────────────────────────────────

func cmdClearThumbnail(ctx *ext.Context, u *ext.Update) error {
	if !hasThumbnailStored() {
		reply(ctx, u, "📸 कोणताही thumbnail set नाही.")
		return dispatcher.EndGroups
	}
	thumbnailStore.Delete("thumb")
	reply(ctx, u, "✅ Thumbnail clear केला. Video uploads आता thumbnail शिवाय जातील.")
	return dispatcher.EndGroups
}

// ── /showthumbnail ────────────────────────────────────────────────────────────

func cmdShowThumbnail(ctx *ext.Context, u *ext.Update) error {
	if !hasThumbnailStored() {
		reply(ctx, u, "📸 कोणताही thumbnail set नाही.\nSet करायला: photo ला reply करून `/setthumbnail` पाठवा.")
		return dispatcher.EndGroups
	}
	reply(ctx, u, "✅ Thumbnail set आहे.\n\nसगळ्या `/userbotdl` video uploads ला हा thumbnail लागतो.\nकाढायला: /clearthumbnail")
	return dispatcher.EndGroups
}

// ── applyReplaces ─────────────────────────────────────────────────────────────

func applyReplaces(caption string) string {
	captionReplaceStore.Range(func(k, v interface{}) bool {
		caption = strings.ReplaceAll(caption, k.(string), v.(string))
		return true
	})
	return caption
}

// ── /adminstream <messageID> ──────────────────────────────────────────────────
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

// ══════════════════════════════════════════════════════════════════════════════
// /userbotdl — Main Command
// ══════════════════════════════════════════════════════════════════════════════

func cmdUserbotDL(ctx *ext.Context, u *ext.Update) error {
	a := args(u)

	if len(a) < 2 {
		reply(ctx, u,
			"📥 *Usage:*\n\n"+
				"`/userbotdl <chatID> <msgID>`\n"+
				"→ Single message\n\n"+
				"`/userbotdl <chatID> <start> <end>`\n"+
				"→ Range of messages\n\n"+
				"`/userbotdl <chatID> <topicID> <start> <end>`\n"+
				"→ Topic + Range\n\n"+
				"*Examples:*\n"+
				"`/userbotdl 2479194095 5654`\n"+
				"`/userbotdl 2479194095 100 200`\n"+
				"`/userbotdl 2479194095 123 100 200`\n\n"+
				"Caption replace rules: /listreplace\n"+
				"Rules add करा: `/setreplace old=new`\n"+
				"Stop करायला: /stopuserbotdl")
		return dispatcher.EndGroups
	}

	// chatID parse
	chatIDRaw, err := strconv.ParseInt(a[0], 10, 64)
	if err != nil || chatIDRaw == 0 {
		reply(ctx, u, "❌ Invalid chatID.\nt.me/c/ वरील positive ID वापरा.\nExample: `2479194095`")
		return dispatcher.EndGroups
	}
	var chatID int64
	if chatIDRaw > 0 {
		chatID = -1000000000000 - chatIDRaw
	} else {
		chatID = chatIDRaw
	}

	var topicID int
	var startMsg int
	var endMsg int

	switch len(a) {
	case 2:
		msgID, err := strconv.Atoi(a[1])
		if err != nil || msgID < 1 {
			reply(ctx, u, "❌ Invalid messageID. Positive number असायला हवे.")
			return dispatcher.EndGroups
		}
		startMsg = msgID
		endMsg = msgID

	case 3:
		s, err1 := strconv.Atoi(a[1])
		e, err2 := strconv.Atoi(a[2])
		if err1 != nil || err2 != nil || s < 1 || e < 1 {
			reply(ctx, u, "❌ Invalid range. दोन्ही positive numbers असायला हवेत.")
			return dispatcher.EndGroups
		}
		if s > e {
			reply(ctx, u, fmt.Sprintf("❌ Start (%d) > End (%d) — start ≤ end असायला हवे.", s, e))
			return dispatcher.EndGroups
		}
		startMsg = s
		endMsg = e

	case 4:
		t, err1 := strconv.Atoi(a[1])
		s, err2 := strconv.Atoi(a[2])
		e, err3 := strconv.Atoi(a[3])
		if err1 != nil || err2 != nil || err3 != nil || t < 1 || s < 1 || e < 1 {
			reply(ctx, u, "❌ Invalid arguments.\nFormat: `/userbotdl <chatID> <topicID> <start> <end>`\nसगळे positive numbers असायला हवेत.")
			return dispatcher.EndGroups
		}
		if s > e {
			reply(ctx, u, fmt.Sprintf("❌ Start (%d) > End (%d) — start ≤ end असायला हवे.", s, e))
			return dispatcher.EndGroups
		}
		topicID = t
		startMsg = s
		endMsg = e

	default:
		reply(ctx, u, "❌ जास्त arguments. Format बघा: `/userbotdl`")
		return dispatcher.EndGroups
	}

	userbotClient := MainUserbotClient
	if userbotClient == nil {
		reply(ctx, u, "❌ Userbot चालू नाही. Admin ला सांगा userbot session set करायला.")
		return dispatcher.EndGroups
	}
	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	// आधी एखादा process चालू असेल तर warn करा
	if hasActiveProcess() {
		reply(ctx, u, "⚠️ आधीच एक `/userbotdl` process चालू आहे.\nथांबवायला: /stopuserbotdl\n\nनवीन process सुरू होत आहे...")
		stopActiveProcess()
		time.Sleep(500 * time.Millisecond)
	}

	adminChatID := u.EffectiveChat().GetID()
	botPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)

	sourcePeerForSend := getChannelPeerIfBotAdmin(ctx, botClient, chatID)

	var sendChannelPeer *tg.InputPeerChannel
	if scID, ok := getSendChannelID(); ok {
		if peer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, scID); err == nil {
			sendChannelPeer = &tg.InputPeerChannel{
				ChannelID:  peer.ChannelID,
				AccessHash: peer.AccessHash,
			}
		}
	}

	totalMsgs := endMsg - startMsg + 1
	rangeStr := ""
	if startMsg == endMsg {
		rangeStr = fmt.Sprintf("Msg `%d`", startMsg)
	} else {
		rangeStr = fmt.Sprintf("Msgs `%d` → `%d` (%d total)", startMsg, endMsg, totalMsgs)
	}
	topicStr := ""
	if topicID > 0 {
		topicStr = fmt.Sprintf("\n🗂 Topic: `%d`", topicID)
	}
	channelStr := ""
	if sourcePeerForSend != nil {
		channelStr = "\n📢 Source channel मध्ये पण send होईल (bot admin आहे)"
	}
	watermarkStr := ""
	if wmText, ok := getWatermarkText(); ok {
		watermarkStr = fmt.Sprintf("\n💧 Watermark: `%s`", wmText)
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
		"⏳ Starting...\n📡 Channel: `%d`\n📋 %s%s%s%s\n\n🛑 Stop: /stopuserbotdl",
		chatIDRaw, rangeStr, topicStr, channelStr, watermarkStr,
	)), nil)

	sentMsgID := 0
	if sentMsg != nil {
		sentMsgID = sentMsg.ID
	}

	// ── Goroutine: process all messages ──────────────────────────────────────
	go func() {
		// [NEW] Cancel context — /stopuserbotdl ने cancel करता येतो
		bgCtx, bgCancel := context.WithCancel(context.Background())
		setActiveCancel(bgCancel)
		defer func() {
			bgCancel()
			clearActiveCancel()
		}()

		var lastEdit time.Time

		editProgress := func(text string) {
			if sentMsgID == 0 {
				return
			}
			if time.Since(lastEdit) < 3*time.Second {
				return
			}
			lastEdit = time.Now()
			editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = botClient.API().MessagesEditMessage(editCtx, &tg.MessagesEditMessageRequest{
				Peer:    botPeer,
				ID:      sentMsgID,
				Message: text,
			})
		}

		deleteProgress := func() {
			if sentMsgID == 0 {
				return
			}
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = botClient.API().MessagesDeleteMessages(delCtx, &tg.MessagesDeleteMessagesRequest{
				Revoke: true,
				ID:     []int{sentMsgID},
			})
		}

		successCount := 0
		skipCount := 0
		errorCount := 0
		stopped := false

		summaryMsgID := 0
		errorLines := make(map[string][]int)
		errorOrder := []string{}

		buildSummaryText := func() string {
			statusLine := ""
			if stopped {
				statusLine = "\n🛑 *Process stopped*"
			}
			s := fmt.Sprintf(
				"📊 *Summary*\n📡 `%d` | Range: `%d`→`%d`\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d%s",
				chatIDRaw, startMsg, endMsg,
				successCount, skipCount, errorCount, statusLine,
			)
			if len(errorLines) > 0 {
				s += "\n\n*Errors:*"
				for _, errText := range errorOrder {
					ids := errorLines[errText]
					if len(ids) == 1 {
						s += fmt.Sprintf("\n• Msg `%d` — `%s`", ids[0], errText)
					} else {
						idStrs := make([]string, len(ids))
						for i, id := range ids {
							idStrs[i] = fmt.Sprintf("`%d`", id)
						}
						s += fmt.Sprintf("\n• Msgs %s — `%s`",
							strings.Join(idStrs, ", "), errText)
					}
				}
			}
			return s
		}

		// [FIX] summaryMsgID parse — सगळे possible update types handle करतो
		updateSummary := func() {
			text := buildSummaryText()
			if summaryMsgID == 0 {
				sCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer sCancel()
				sentSummary, err := botClient.API().MessagesSendMessage(sCtx, &tg.MessagesSendMessageRequest{
					Peer:      botPeer,
					Message:   text,
					RandomID:  rand.Int63(),
					NoWebpage: true,
				})
				if err == nil {
					// [FIX] सगळे MTProto update types handle करा
					summaryMsgID = extractMessageID(sentSummary)
				}
			} else {
				eCtx, eCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer eCancel()
				_, _ = botClient.API().MessagesEditMessage(eCtx, &tg.MessagesEditMessageRequest{
					Peer:      botPeer,
					ID:        summaryMsgID,
					Message:   text,
					NoWebpage: true,
				})
			}
		}

		for msgID := startMsg; msgID <= endMsg; msgID++ {

			// Stop check
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			currentIdx := msgID - startMsg + 1
			editProgress(fmt.Sprintf(
				"⏳ Processing %d/%d\n📡 Channel: `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n\n🛑 Stop: /stopuserbotdl",
				currentIdx, totalMsgs, chatIDRaw, msgID,
				successCount, skipCount, errorCount,
			))

			const maxRetries = 2
			var err error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if bgCtx.Err() != nil {
					break
				}

				if attempt > 1 {
					editProgress(fmt.Sprintf(
						"🔄 Retry %d/%d\n📌 Msg: `%d`",
						attempt, maxRetries, msgID,
					))
					time.Sleep(5 * time.Second)
				}

				err = processSingleMessage(
					bgCtx,
					userbotClient,
					botClient,
					chatID,
					chatIDRaw,
					msgID,
					topicID,
					botPeer,
					sourcePeerForSend,
					sendChannelPeer,
					editProgress,
				)

				if err == nil {
					break
				}

				if bgCtx.Err() != nil {
					break
				}
			}

			// Stop झाला असेल तर
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			if err != nil {
				errorCount++
				errKey := err.Error()
				if _, exists := errorLines[errKey]; !exists {
					errorOrder = append(errorOrder, errKey)
				}
				errorLines[errKey] = append(errorLines[errKey], msgID)
				updateSummary()
				if totalMsgs > 1 {
					time.Sleep(2 * time.Second)
				}
				continue
			}

			successCount++
			updateSummary()

			if totalMsgs > 1 && msgID < endMsg {
				time.Sleep(1 * time.Second)
			}
		}

		deleteProgress()
		updateSummary()
	}()

	return dispatcher.EndGroups
}

// ── extractMessageID ─────────────────────────────────────────────────────────
// [FIX] MessagesSendMessage / MessagesSendMedia च्या result मधून message ID काढतो.
// सगळे possible MTProto update types handle करतो.

func extractMessageID(result tg.UpdatesClass) int {
	if result == nil {
		return 0
	}
	switch upd := result.(type) {
	case *tg.UpdateShortSentMessage:
		// Bot private chat मध्ये हा येतो — direct ID
		return upd.ID

	case *tg.Updates:
		// सगळे updates scan करा
		for _, u := range upd.Updates {
			switch mu := u.(type) {
			case *tg.UpdateMessageID:
				if mu.ID > 0 {
					return mu.ID
				}
			case *tg.UpdateNewMessage:
				if m, ok := mu.Message.(*tg.Message); ok && m.ID > 0 {
					return m.ID
				}
			case *tg.UpdateNewChannelMessage:
				if m, ok := mu.Message.(*tg.Message); ok && m.ID > 0 {
					return m.ID
				}
			}
		}

	case *tg.UpdatesCombined:
		for _, u := range upd.Updates {
			switch mu := u.(type) {
			case *tg.UpdateMessageID:
				if mu.ID > 0 {
					return mu.ID
				}
			case *tg.UpdateNewMessage:
				if m, ok := mu.Message.(*tg.Message); ok && m.ID > 0 {
					return m.ID
				}
			case *tg.UpdateNewChannelMessage:
				if m, ok := mu.Message.(*tg.Message); ok && m.ID > 0 {
					return m.ID
				}
			}
		}

	case *tg.UpdateShort:
		if mu, ok := upd.Update.(*tg.UpdateMessageID); ok && mu.ID > 0 {
			return mu.ID
		}
	}
	return 0
}

// ── inputMediaDocFromUpdates ──────────────────────────────────────────────────
// [FIX] UpdatesCombined पण handle करतो

func inputMediaDocFromUpdates(result tg.UpdatesClass) tg.InputMediaClass {
	if result == nil {
		return nil
	}
	var updates []tg.UpdateClass
	switch u := result.(type) {
	case *tg.Updates:
		updates = u.Updates
	case *tg.UpdatesCombined:
		updates = u.Updates
	default:
		return nil
	}
	for _, upd := range updates {
		var m *tg.Message
		switch u := upd.(type) {
		case *tg.UpdateNewMessage:
			if msg, ok := u.Message.(*tg.Message); ok {
				m = msg
			}
		case *tg.UpdateNewChannelMessage:
			if msg, ok := u.Message.(*tg.Message); ok {
				m = msg
			}
		}
		if m != nil {
			if mmd, ok := m.Media.(*tg.MessageMediaDocument); ok {
				if d, ok := mmd.Document.(*tg.Document); ok {
					return &tg.InputMediaDocument{
						ID: &tg.InputDocument{
							ID:            d.ID,
							AccessHash:    d.AccessHash,
							FileReference: d.FileReference,
						},
					}
				}
			}
		}
	}
	return nil
}

// ── processSingleMessage ──────────────────────────────────────────────────────

func processSingleMessage(
	bgCtx context.Context,
	userbotClient *gotgproto.Client,
	botClient *gotgproto.Client,
	chatID int64,
	chatIDRaw int64,
	msgID int,
	topicID int,
	botPeer tg.InputPeerClass,
	sourcePeer *tg.InputPeerChannel,
	sendChannelPeer *tg.InputPeerChannel,
	editProgress func(string),
) error {

	// ── Step 1: Fetch message ─────────────────────────────────────────────
	tgMsg, err := utils.GetTGMessageInChannel(bgCtx, userbotClient, msgID, chatID)
	if err != nil {
		return fmt.Errorf("message fetch failed: %w", err)
	}

	// ── Step 2: Caption prepare ───────────────────────────────────────────
	originalCaption := strings.TrimSpace(tgMsg.Message)
	finalCaption := applyReplaces(originalCaption)

	// ── Step 3: No media → text message copy ─────────────────────────────
	if tgMsg.Media == nil {
		if sendChannelPeer != nil {
			scCtx, scCancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer scCancel()
			scReq := &tg.MessagesSendMessageRequest{
				Peer:     sendChannelPeer,
				Message:  finalCaption,
				RandomID: rand.Int63(),
			}
			if topicID > 0 {
				scReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
			}
			_, _ = botClient.API().MessagesSendMessage(scCtx, scReq)

			go func() {
				lCtx, lCancel := context.WithTimeout(context.Background(), 1*time.Minute)
				defer lCancel()
				logCh, lerr := utils.GetLogChannelPeer(lCtx, botClient.API(), botClient.PeerStorage)
				if lerr != nil {
					return
				}
				_, _ = botClient.API().MessagesSendMessage(lCtx, &tg.MessagesSendMessageRequest{
					Peer:     &tg.InputPeerChannel{ChannelID: logCh.ChannelID, AccessHash: logCh.AccessHash},
					Message:  finalCaption,
					RandomID: rand.Int63(),
					Silent:   true,
				})
			}()
		} else {
			textContent := fmt.Sprintf(
				"📥 Userbot DL\n📌 Chat: `%d` | Msg: `%d`\n\n%s",
				chatIDRaw, msgID, finalCaption,
			)
			sendCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()
			_, err = botClient.API().MessagesSendMessage(sendCtx, &tg.MessagesSendMessageRequest{
				Peer:     botPeer,
				Message:  textContent,
				RandomID: rand.Int63(),
			})
			if err != nil {
				return fmt.Errorf("text send failed: %w", err)
			}

			if sourcePeer != nil {
				sendCtx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Minute)
				defer cancel2()
				sendReq := &tg.MessagesSendMessageRequest{
					Peer:     sourcePeer,
					Message:  finalCaption,
					RandomID: rand.Int63(),
				}
				if topicID > 0 {
					sendReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
				}
				_, _ = botClient.API().MessagesSendMessage(sendCtx2, sendReq)
			}
		}
		return nil
	}

	// ── Step 4: Media → download + reupload ──────────────────────────────
	file, err := utils.FileFromMessageInChannel(bgCtx, userbotClient, msgID, chatID)
	if err != nil {
		return fmt.Errorf("file metadata error: %w", err)
	}

	editProgress(fmt.Sprintf(
		"📥 Downloading...\n📄 `%s`\n💾 %s\n📌 Chat: `%d` | Msg: `%d`\n░░░░░░░░░░ 0%%",
		file.FileName, formatSize(file.FileSize), chatIDRaw, msgID,
	))

	stallCtx, stallCancel := context.WithCancel(bgCtx)
	defer stallCancel()
	stallTimer := time.AfterFunc(5*time.Minute, stallCancel)

	onDownloadProgress := func(written, total int64) {
		stallTimer.Reset(5 * time.Minute)
		if total <= 0 {
			return
		}
		pct := written * 100 / total
		bar := progressBar(int(pct))
		editProgress(fmt.Sprintf(
			"📥 Downloading...\n📄 `%s`\n💾 %s / %s\n%s %d%%\n📌 Msg: `%d`",
			file.FileName,
			formatSize(written), formatSize(total),
			bar, pct, msgID,
		))
	}

	onUploadProgress := func(written, total int64) {
		if total <= 0 {
			return
		}
		pct := written * 100 / total
		bar := progressBar(int(pct))
		editProgress(fmt.Sprintf(
			"📤 Uploading...\n📄 `%s`\n💾 %s / %s\n%s %d%%\n📌 Msg: `%d`",
			file.FileName,
			formatSize(written), formatSize(total),
			bar, pct, msgID,
		))
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer upCancel()

	// ── Step 4b: Watermark apply (FFmpeg) ────────────────────────────────
	// Watermark set असेल तर download → FFmpeg overlay → reupload
	wmText, wmOk := getWatermarkText()

	var uploadedFile tg.InputFileClass

	if wmOk && isVideoFile(file) {
		// Watermark path: download to temp → ffmpeg → upload processed file
		editProgress(fmt.Sprintf(
			"📥 Downloading for watermark...\n📄 `%s`\n💾 %s\n📌 Msg: `%d`",
			file.FileName, formatSize(file.FileSize), msgID,
		))

		uploadedFile, err = downloadWatermarkAndReupload(
			stallCtx,
			upCtx,
			userbotClient,
			botClient,
			file,
			wmText,
			onDownloadProgress,
			onUploadProgress,
			editProgress,
			msgID,
		)
	} else {
		uploadedFile, err = downloadAndReupload(
			stallCtx,
			upCtx,
			userbotClient,
			botClient,
			file,
			onDownloadProgress,
			onUploadProgress,
		)
	}

	stallTimer.Stop()

	if err != nil {
		if stallCtx.Err() != nil && bgCtx.Err() == nil {
			return fmt.Errorf("download stalled (5min no progress) | Retry: /userbotdl %d %d", chatIDRaw, msgID)
		}
		return fmt.Errorf("download/upload failed: %w", err)
	}

	// ── Step 5: Build media + attributes ─────────────────────────────────
	// ── Step 5: Build media + attributes ─────────────────────────────────
	attributes := []tg.DocumentAttributeClass{
		&tg.DocumentAttributeFilename{FileName: file.FileName},
	}
	isVideo := false
	if doc, ok := tgMsg.Media.(*tg.MessageMediaDocument); ok {
		if d, ok := doc.Document.(*tg.Document); ok {
			for _, attr := range d.Attributes {
				switch attr.(type) {
				case *tg.DocumentAttributeVideo,
					*tg.DocumentAttributeAudio,
					*tg.DocumentAttributeAnimated:
					attributes = append(attributes, attr)
				}
				if _, ok := attr.(*tg.DocumentAttributeVideo); ok {
					isVideo = true
				}
			}
		}
	}
	if !isVideo {
		mime := strings.ToLower(file.MimeType)
		if strings.HasPrefix(mime, "video/") {
			isVideo = true
		} else {
			switch strings.ToLower(filepath.Ext(file.FileName)) {
			case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv", ".m4v", ".ts", ".3gp":
				isVideo = true
			}
		}
	}

	// ── Thumbnail ─────────────────────────────────────────────────────────
	var thumbSrc []byte
	if b, ok := getStoredThumbnailBytes(); ok && len(b) > 0 {
		thumbSrc = b
	} else if isVideo {
		if doc, ok := tgMsg.Media.(*tg.MessageMediaDocument); ok {
			if d, ok := doc.Document.(*tg.Document); ok {
				var bestType string
				var bestW int
				for _, sz := range d.Thumbs {
					if ps, ok := sz.(*tg.PhotoSize); ok && ps.W > bestW {
						bestW = ps.W
						bestType = ps.Type
					}
				}
				if bestType != "" {
					loc := &tg.InputDocumentFileLocation{
						ID:            d.ID,
						AccessHash:    d.AccessHash,
						FileReference: d.FileReference,
						ThumbSize:     bestType,
					}
					dlCtx, dlCancel := context.WithTimeout(bgCtx, 30*time.Second)
					defer dlCancel()
					var buf bytes.Buffer
					if _, dlErr := downloader.NewDownloader().Download(userbotClient.API(), loc).Stream(dlCtx, &buf); dlErr == nil {
						thumbSrc = buf.Bytes()
					}
				}
			}
		}
	}

	inputMedia := &tg.InputMediaUploadedDocument{
		File:       uploadedFile,
		MimeType:   file.MimeType,
		Attributes: attributes,
	}
	if isVideo && len(thumbSrc) > 0 {
		thumbUpCtx, thumbUpCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer thumbUpCancel()
		freshThumb, thumbErr := uploader.NewUploader(botClient.API()).FromReader(
			thumbUpCtx, "thumb.jpg", bytes.NewReader(thumbSrc),
		)
		if thumbErr == nil {
			inputMedia.SetThumb(freshThumb)
		} else {
			editProgress(fmt.Sprintf("⚠️ Thumb upload failed: %v", thumbErr))
		}
	}

	// ── Step 6 + 7 + 8: Send targets ─────────────────────────────────────
	if sendChannelPeer != nil {
		fileCaption := finalCaption
		if fileCaption == "" {
			fileCaption = file.FileName
		}

		scCtx, scCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer scCancel()
		scReq := &tg.MessagesSendMediaRequest{
			Peer:     sendChannelPeer,
			Media:    inputMedia,
			Message:  fileCaption,
			RandomID: rand.Int63(),
		}
		if topicID > 0 {
			scReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
		}
		scResult, _ := botClient.API().MessagesSendMedia(scCtx, scReq)

		// [FIX] Log channel: doc reference extract — fallback safe
		if markCopied(chatID, msgID) {
			go func() {
				logMedia := tg.InputMediaClass(inputMedia)
				if dm := inputMediaDocFromUpdates(scResult); dm != nil {
					logMedia = dm
				}
				copyToLogChannelUploaded(tgMsg, logMedia)
			}()
		}
	} else {
		adminCaption := fmt.Sprintf(
			"📥 Userbot DL\n📌 Chat: `%d` | Msg: `%d`\n📄 `%s`\n💾 %s\n🔒 Forward protected",
			chatIDRaw, msgID, file.FileName, formatSize(file.FileSize),
		)
		adminCaption = applyReplaces(adminCaption)

		sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer sendCancel()
		adminResult, err := botClient.API().MessagesSendMedia(sendCtx, &tg.MessagesSendMediaRequest{
			Peer:       botPeer,
			Media:      inputMedia,
			Message:    adminCaption,
			Noforwards: true,
			RandomID:   rand.Int63(),
		})
		if err != nil {
			return fmt.Errorf("send to admin failed: %w", err)
		}

		// [FIX] Log channel: doc reference extract — fallback safe
		if markCopied(chatID, msgID) {
			go func() {
				logMedia := tg.InputMediaClass(inputMedia)
				if dm := inputMediaDocFromUpdates(adminResult); dm != nil {
					logMedia = dm
				}
				copyToLogChannelUploaded(tgMsg, logMedia)
			}()
		}

		if sourcePeer != nil {
			channelCaption := finalCaption
			if channelCaption == "" {
				channelCaption = file.FileName
			}
			// [FIX] Source channel: always use doc reference (InputFile already consumed)
			logMedia := tg.InputMediaClass(inputMedia)
			if dm := inputMediaDocFromUpdates(adminResult); dm != nil {
				logMedia = dm
			}
			sendCtx2, sendCancel2 := context.WithTimeout(context.Background(), 10*time.Minute)
			defer sendCancel2()
			chReq := &tg.MessagesSendMediaRequest{
				Peer:     sourcePeer,
				Media:    logMedia,
				Message:  channelCaption,
				RandomID: rand.Int63(),
			}
			if topicID > 0 {
				chReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
			}
			_, _ = botClient.API().MessagesSendMedia(sendCtx2, chReq)
		}
	}

	return nil
}

// ── isVideoFile ───────────────────────────────────────────────────────────────
// File video आहे का check — watermark साठी

func isVideoFile(file *types.File) bool {
	mime := strings.ToLower(file.MimeType)
	if strings.HasPrefix(mime, "video/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(file.FileName)) {
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv", ".m4v", ".ts", ".3gp":
		return true
	}
	return false
}

// ── downloadWatermarkAndReupload ──────────────────────────────────────────────
// Video download → FFmpeg watermark overlay → reupload
// Watermark: bottom-right corner, white text, semi-transparent

func downloadWatermarkAndReupload(
	dlCtx context.Context,
	upCtx context.Context,
	dlClient *gotgproto.Client,
	upClient *gotgproto.Client,
	file *types.File,
	wmText string,
	onDownloadProgress func(written, total int64),
	onUploadProgress func(written, total int64),
	editProgress func(string),
	msgID int,
) (tg.InputFileClass, error) {

	// ── Download to temp file ─────────────────────────────────────────────
	ext := filepath.Ext(file.FileName)
	if ext == "" {
		ext = ".mp4"
	}

	tmpInput, err := os.CreateTemp("", "wm-input-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("temp input file create failed: %w", err)
	}
	inputPath := tmpInput.Name()
	defer os.Remove(inputPath)

	var dlWriter io.Writer = tmpInput
	if onDownloadProgress != nil {
		dlWriter = &progressWriter{
			w:          tmpInput,
			total:      file.FileSize,
			onProgress: onDownloadProgress,
		}
	}

	d := downloader.NewDownloader()
	_, err = d.Download(dlClient.API(), file.Location).Stream(dlCtx, dlWriter)
	tmpInput.Close()
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// ── FFmpeg watermark overlay ──────────────────────────────────────────
	editProgress(fmt.Sprintf("💧 Applying watermark...\n📄 `%s`\n📌 Msg: `%d`", file.FileName, msgID))

	tmpOutput, err := os.CreateTemp("", "wm-output-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("temp output file create failed: %w", err)
	}
	outputPath := tmpOutput.Name()
	tmpOutput.Close()
	defer os.Remove(outputPath)

	// FFmpeg command:
	// - drawtext filter: bottom-right corner, white text, semi-transparent black box background
	// - font size 36, padding 10px from edges
	// - copy codec (no re-encode) — fast processing
	// Note: fontconfig available असेल तर font=sans पण चालेल
	drawtextFilter := fmt.Sprintf(
		"drawtext=text='%s':"+
			"fontcolor=white:"+
			"fontsize=36:"+
			"box=1:boxcolor=black@0.4:boxborderw=8:"+
			"x=w-tw-20:y=h-th-20",
		strings.ReplaceAll(wmText, "'", "\\'"), // single quote escape
	)

	// -c copy वापरतो — re-encode नाही → fast
	// काही formats मध्ये filter + copy एकत्र चालत नाही, त्यासाठी fallback
	args := []string{
		"-i", inputPath,
		"-vf", drawtextFilter,
		"-c:a", "copy",     // audio copy
		"-c:v", "libx264",  // video re-encode (drawtext साठी आवश्यक)
		"-preset", "ultrafast", // fastest encoding
		"-crf", "23",       // quality — 23 = good balance
		"-movflags", "+faststart",
		"-y", // overwrite
		outputPath,
	}

	ffmpegCtx, ffmpegCancel := context.WithTimeout(upCtx, 25*time.Minute)
	defer ffmpegCancel()

	cmd := exec.CommandContext(ffmpegCtx, "ffmpeg", args...)
	ffmpegOut, ffmpegErr := cmd.CombinedOutput()
	if ffmpegErr != nil {
		// FFmpeg failed — output log सह error return
		errMsg := string(ffmpegOut)
		if len(errMsg) > 300 {
			errMsg = errMsg[len(errMsg)-300:] // last 300 chars (most relevant)
		}
		utils.Logger.Error("FFmpeg watermark failed",
			zap.String("file", file.FileName),
			zap.String("output", errMsg),
			zap.Error(ffmpegErr),
		)
		return nil, fmt.Errorf("FFmpeg watermark failed: %w\nFFmpeg: %s", ffmpegErr, errMsg)
	}

	// Output file stat
	outInfo, err := os.Stat(outputPath)
	if err != nil || outInfo.Size() == 0 {
		return nil, fmt.Errorf("FFmpeg output file empty or missing")
	}

	// ── Upload processed file ─────────────────────────────────────────────
	editProgress(fmt.Sprintf("📤 Uploading watermarked...\n📄 `%s`\n📌 Msg: `%d`", file.FileName, msgID))

	readFile, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("output file open failed: %w", err)
	}
	defer readFile.Close()

	totalSize := outInfo.Size()
	var upReader io.Reader = readFile
	if onUploadProgress != nil {
		upReader = &progressWriter{
			w:          readFile,
			total:      totalSize,
			onProgress: onUploadProgress,
		}
	}

	up := uploader.NewUploader(upClient.API()).WithPartSize(256 * 1024)
	uploadedFile, err := up.FromReader(upCtx, file.FileName, upReader)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}

	return uploadedFile, nil
}

// ── getChannelPeerIfBotAdmin ──────────────────────────────────────────────────

func getChannelPeerIfBotAdmin(
	ctx *ext.Context,
	botClient *gotgproto.Client,
	channelID int64,
) *tg.InputPeerChannel {
	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, channelID)
	if err != nil {
		return nil
	}

	if botClient.Self == nil {
		return nil
	}
	botID := botClient.Self.ID

	checkCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	participants, err := botClient.API().ChannelsGetParticipants(checkCtx, &tg.ChannelsGetParticipantsRequest{
		Channel: channelPeer,
		Filter:  &tg.ChannelParticipantsAdmins{},
		Offset:  0,
		Limit:   200,
	})
	if err != nil {
		return nil
	}

	chParticipants, ok := participants.(*tg.ChannelsChannelParticipants)
	if !ok {
		return nil
	}

	for _, p := range chParticipants.Participants {
		switch v := p.(type) {
		case *tg.ChannelParticipantAdmin:
			if v.UserID == botID {
				return &tg.InputPeerChannel{
					ChannelID:  channelPeer.ChannelID,
					AccessHash: channelPeer.AccessHash,
				}
			}
		case *tg.ChannelParticipantCreator:
			if v.UserID == botID {
				return &tg.InputPeerChannel{
					ChannelID:  channelPeer.ChannelID,
					AccessHash: channelPeer.AccessHash,
				}
			}
		}
	}

	return nil
}

// ── downloadAndReupload ───────────────────────────────────────────────────────

func downloadAndReupload(
	dlCtx context.Context,
	upCtx context.Context,
	dlClient *gotgproto.Client,
	upClient *gotgproto.Client,
	file *types.File,
	onDownloadProgress func(written, total int64),
	onUploadProgress func(written, total int64),
) (tg.InputFileClass, error) {

	tmpFile, err := os.CreateTemp("", "userbotdl-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("temp file create failed: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var dlWriter io.Writer = tmpFile
	if onDownloadProgress != nil {
		dlWriter = &progressWriter{
			w:          tmpFile,
			total:      file.FileSize,
			onProgress: onDownloadProgress,
		}
	}

	d := downloader.NewDownloader()
	_, err = d.Download(dlClient.API(), file.Location).Stream(dlCtx, dlWriter)
	tmpFile.Close()
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("temp file stat failed: %w", err)
	}
	totalSize := info.Size()

	readFile, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("temp file reopen failed: %w", err)
	}
	defer readFile.Close()

	var upReader io.Reader = readFile
	if onUploadProgress != nil {
		upReader = &progressWriter{
			w:          readFile,
			total:      totalSize,
			onProgress: onUploadProgress,
		}
	}

	up := uploader.NewUploader(upClient.API()).WithPartSize(256 * 1024)
	uploadedFile, err := up.FromReader(upCtx, file.FileName, upReader)
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

func (p *progressWriter) Read(b []byte) (int, error) {
	r, ok := p.w.(io.Reader)
	if !ok {
		return 0, fmt.Errorf("underlying writer does not implement Reader")
	}
	n, err := r.Read(b)
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
