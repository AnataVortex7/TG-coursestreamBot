// internal/commands/admin_stream.go
package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
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

// ── Caption Replace Store (server memory only) ────────────────────────────────
// Key: original word, Value: replacement word
// sync.Map — concurrent safe, no DB needed

var captionReplaceStore sync.Map // map[string]string

// ── Send Channel Store ────────────────────────────────────────────────────────
// /userbotdl चे files + text msgs या channel ला पण send होतात
// Server restart वर reset होतो (in-memory only)

var sendChannelStore sync.Map // key: "id", value: int64

func getSendChannelID() (int64, bool) {
	v, ok := sendChannelStore.Load("id")
	if !ok {
		return 0, false
	}
	return v.(int64), true
}

// ── Thumbnail Store ───────────────────────────────────────────────────────────
// /setthumbnail ने set केलेला photo — video upload साठी thumbnail
// bytes म्हणून store — प्रत्येक upload ला fresh InputFile तयार करतो
// (InputFile single-session reference असतो, reuse करता येत नाही)
// Server restart वर reset होतो (in-memory only)

var thumbnailStore sync.Map // key: "thumb", value: []byte

func getStoredThumbnailBytes() ([]byte, bool) {
	v, ok := thumbnailStore.Load("thumb")
	if !ok {
		return nil, false
	}
	return v.([]byte), true
}

// Thumbnail set आहे का check (bytes length > 0)
func hasThumbnailStored() bool {
	_, ok := thumbnailStore.Load("thumb")
	return ok
}

// ── Register ──────────────────────────────────────────────────────────────────

func (m *command) LoadAdminStream(d dispatcher.Dispatcher) {
	log := m.log.Named("admin_stream")
	defer log.Sugar().Info("Loaded /adminstream /userbotdl /setreplace /listreplace /delreplace /clearreplace /setsendchannel /showsendchannel /clearsendchannel commands")

	d.AddHandler(handlers.NewCommand("adminstream", adminOnly(cmdAdminStream)))
	d.AddHandler(handlers.NewCommand("userbotdl", adminOnly(cmdUserbotDL)))
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
}

// ── /setreplace <old=new> [old2=new2] ... ─────────────────────────────────────
// Caption मधील words replace करायचे rules set करतो.
// Usage: /setreplace hello=नमस्ते bot=बॉट
// Multiple pairs एकाच command मध्ये देता येतात.

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
// /userbotdl चे सगळे files + text msgs या channel ला पण send होतात
// Channel ID: t.me/c/ वरील positive number (उदा. 2479194095)
// Bot त्या channel चा owner/admin असणे आवश्यक आहे

func cmdSetSendChannel(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		curID, ok := getSendChannelID()
		if ok {
			// internal format → user-facing positive ID
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

	// Convert to internal TG format
	internalID := -1000000000000 - rawID

	// Bot access check
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
// Photo reply करा command ला → तो photo सगळ्या video uploads चा thumbnail होतो
// Usage: photo ला reply करून /setthumbnail पाठवा

func cmdSetThumbnail(ctx *ext.Context, u *ext.Update) error {
	tgMsg := u.EffectiveMessage.Message
	if tgMsg == nil {
		reply(ctx, u, "❌ Message मिळाला नाही.")
		return dispatcher.EndGroups
	}

	// ReplyTo header मिळवा
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

	// Reply message fetch करा — same chat मधून
	chatID := u.EffectiveChat().GetID()
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fetchCancel()

	inputPeer := botClient.PeerStorage.GetInputPeerById(chatID)
	var replyMsg *tg.Message

	// Chat type नुसार fetch
	switch peer := inputPeer.(type) {
	case *tg.InputPeerChannel:
		res, err := botClient.API().ChannelsGetMessages(fetchCtx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: replyMsgID}},
		})
		if err != nil {
			reply(ctx, u, fmt.Sprintf("❌ Reply message fetch failed: `%s`", err.Error()))
			return dispatcher.EndGroups
		}
		msgs := res.(*tg.MessagesChannelMessages)
		if len(msgs.Messages) == 0 {
			reply(ctx, u, "❌ Reply message सापडला नाही.")
			return dispatcher.EndGroups
		}
		m, ok := msgs.Messages[0].(*tg.Message)
		if !ok {
			reply(ctx, u, "❌ Reply message invalid आहे.")
			return dispatcher.EndGroups
		}
		replyMsg = m
	default:
		// Private chat / group
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

	// Download → temp file → upload
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

	// Thumbnail bytes म्हणून store करा — प्रत्येक video upload ला fresh re-upload होईल
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
// Caption मधील सर्व registered words replace करतो.
// Case-sensitive, exact substring replace.

func applyReplaces(caption string) string {
	captionReplaceStore.Range(func(k, v interface{}) bool {
		caption = strings.ReplaceAll(caption, k.(string), v.(string))
		return true
	})
	return caption
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

// ══════════════════════════════════════════════════════════════════════════════
// /userbotdl — Main Command
//
// Formats:
//   /userbotdl <chatID> <msgID>              → single message
//   /userbotdl <chatID> <start> <end>        → range, no topic
//   /userbotdl <chatID> <topicID> <start> <end> → topic + range
//
// Features:
//   - Single / Range support
//   - Caption replace (captionReplaceStore मधील rules apply होतात)
//   - Bot admin असलेल्या channel/group मध्ये पण send
//   - Full error handling per message
// ══════════════════════════════════════════════════════════════════════════════

func cmdUserbotDL(ctx *ext.Context, u *ext.Update) error {
	a := args(u)

	// ── Argument Parsing ──────────────────────────────────────────────────────
	//
	// Format 1: /userbotdl <chatID> <msgID>           → len(a)==2
	// Format 2: /userbotdl <chatID> <start> <end>     → len(a)==3
	// Format 3: /userbotdl <chatID> <topic> <start> <end> → len(a)==4

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
				"Rules add करा: `/setreplace old=new`")
		return dispatcher.EndGroups
	}

	// chatID parse
	chatIDRaw, err := strconv.ParseInt(a[0], 10, 64)
	if err != nil || chatIDRaw == 0 {
		reply(ctx, u, "❌ Invalid chatID.\nt.me/c/ वरील positive ID वापरा.\nExample: `2479194095`")
		return dispatcher.EndGroups
	}
	// Telegram internal channel ID format
	var chatID int64
	if chatIDRaw > 0 {
		chatID = -1000000000000 - chatIDRaw
	} else {
		chatID = chatIDRaw
	}

	var topicID int    // 0 = no topic
	var startMsg int
	var endMsg int

	switch len(a) {
	case 2:
		// Single message: /userbotdl <chatID> <msgID>
		msgID, err := strconv.Atoi(a[1])
		if err != nil || msgID < 1 {
			reply(ctx, u, "❌ Invalid messageID. Positive number असायला हवे.")
			return dispatcher.EndGroups
		}
		startMsg = msgID
		endMsg = msgID

	case 3:
		// Range: /userbotdl <chatID> <start> <end>
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
		// Topic + Range: /userbotdl <chatID> <topicID> <start> <end>
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

	// ── Client checks ─────────────────────────────────────────────────────────
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

	// ── Determine send targets ────────────────────────────────────────────────
	// 1. Admin chat (जिथून command आला) — नेहमी
	// 2. Bot ज्या channel/group मध्ये admin आहे त्या source channel — optional
	adminChatID := u.EffectiveChat().GetID()
	botPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)

	// Source channel मध्ये bot admin आहे का check करा
	sourcePeerForSend := getChannelPeerIfBotAdmin(ctx, botClient, chatID)

	// Send channel peer — /setsendchannel ने set केलेला
	var sendChannelPeer *tg.InputPeerChannel
	if scID, ok := getSendChannelID(); ok {
		if peer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, scID); err == nil {
			sendChannelPeer = &tg.InputPeerChannel{
				ChannelID:  peer.ChannelID,
				AccessHash: peer.AccessHash,
			}
		}
	}

	// ── Summary message ───────────────────────────────────────────────────────
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

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
		"⏳ Starting...\n📡 Channel: `%d`\n📋 %s%s%s",
		chatIDRaw, rangeStr, topicStr, channelStr,
	)), nil)

	sentMsgID := 0
	if sentMsg != nil {
		sentMsgID = sentMsg.ID
	}

	// ── Goroutine: process all messages ──────────────────────────────────────
	go func() {
		// No overall timeout — range complete होईपर्यंत चालू राहील
		bgCtx, bgCancel := context.WithCancel(context.Background())
		defer bgCancel()

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

		// ── Process each message in range ─────────────────────────────────
		successCount := 0
		skipCount    := 0
		errorCount   := 0

		// Summary message — एकच message, edit होत राहील
		summaryMsgID := 0
		// errorLines: map[errorText] -> []msgID (same error group करायला)
		errorLines := make(map[string][]int)
		errorOrder := []string{} // insertion order

		buildSummaryText := func() string {
			s := fmt.Sprintf(
				"📊 *Summary*\n📡 `%d` | Range: `%d`→`%d`\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d",
				chatIDRaw, startMsg, endMsg,
				successCount, skipCount, errorCount,
			)
			if len(errorLines) > 0 {
				s += "\n\n*Errors:*"
				for _, errText := range errorOrder {
					ids := errorLines[errText]
					if len(ids) == 1 {
						s += fmt.Sprintf("\n• Msg `%d` — `%s`", ids[0], errText)
					} else {
						// Multiple same errors — group करा
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

		updateSummary := func() {
			text := buildSummaryText()
			if summaryMsgID == 0 {
				// पहिल्यांदा send करा
				sCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer sCancel()
				sentSummary, err := botClient.API().MessagesSendMessage(sCtx, &tg.MessagesSendMessageRequest{
					Peer:      botPeer,
					Message:   text,
					RandomID:  rand.Int63(),
					NoWebpage: true,
				})
				if err == nil {
					// MTProto response type multiple असू शकतो —
					// सगळे handle करा नाहीतर summaryMsgID कधीच set होत नाही
					switch upd := sentSummary.(type) {
					case *tg.Updates:
						for _, u := range upd.Updates {
							if mu, ok := u.(*tg.UpdateMessageID); ok {
								summaryMsgID = mu.ID
								break
							}
							if mu, ok := u.(*tg.UpdateNewMessage); ok {
								if m, ok := mu.Message.(*tg.Message); ok {
									summaryMsgID = m.ID
								}
								break
							}
						}
					case *tg.UpdateShortSentMessage:
						// Bot chat मध्ये हा type येतो — direct ID असतो
						summaryMsgID = upd.ID
					}
				}
			} else {
				// Edit करा
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

			// Context cancel (manual stop) तर exit
			if bgCtx.Err() != nil {
				deleteProgress()
				updateSummary()
				return
			}

			currentIdx := msgID - startMsg + 1
			editProgress(fmt.Sprintf(
				"⏳ Processing %d/%d\n📡 Channel: `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d",
				currentIdx, totalMsgs, chatIDRaw, msgID,
				successCount, skipCount, errorCount,
			))

			// ── Retry logic: 2 attempts, मग skip ────────────────────────────
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
					break // success — retry loop बाहेर
				}

				// Overall context cancel झाला असेल तर retry नको
				if bgCtx.Err() != nil {
					break
				}
			}

			if err != nil {
				errorCount++
				// Error summary मध्ये add करा — same error group करा
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

			// Range मध्ये messages मध्ये थोडा gap द्या Telegram throttle टाळायला
			if totalMsgs > 1 && msgID < endMsg {
				time.Sleep(1 * time.Second)
			}
		}

		// ── Final summary ──────────────────────────────────────────────────
		deleteProgress()
		updateSummary()
	}()

	return dispatcher.EndGroups
}

// ── processSingleMessage ──────────────────────────────────────────────────────
// inputMediaDocFromUpdates — पहिल्या MessagesSendMedia च्या result मधून
// InputMediaDocument काढतो. InputFile single-use असल्याने log/source channel
// ला second send साठी हे वापरायला हवे.
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

// एका message साठी: fetch → (download+reupload if media) → send to targets
// error return केल्यास caller error count वाढवतो आणि continue करतो

func processSingleMessage(
	bgCtx context.Context,
	userbotClient *gotgproto.Client,
	botClient *gotgproto.Client,
	chatID int64,        // internal TG ID (negative)
	chatIDRaw int64,     // user-facing positive ID (for display)
	msgID int,
	topicID int,
	botPeer tg.InputPeerClass,           // admin chat peer
	sourcePeer *tg.InputPeerChannel,     // source channel peer (nil if bot not admin)
	sendChannelPeer *tg.InputPeerChannel, // /setsendchannel peer (nil if not set)
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
		// sendChannel set आहे का?
		if sendChannelPeer != nil {
			// sendChannel set → फक्त sendChannel + log channel ला send करा
			// Admin ला फक्त progress/error दिसेल, msg नाही

			// sendChannel ला send
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

			// Log channel ला send (text)
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
			// sendChannel नाही → normal: admin ला send
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

			// Source channel असेल तर तिथे पण
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

	// Stall detection — 5 min no progress
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

	uploadedFile, err := downloadAndReupload(
		stallCtx,
		upCtx,
		userbotClient,
		botClient,
		file,
		onDownloadProgress,
		onUploadProgress,
	)
	stallTimer.Stop()

	if err != nil {
		if stallCtx.Err() != nil && bgCtx.Err() == nil {
			return fmt.Errorf("download stalled (5min no progress) | Retry: /userbotdl %d %d", chatIDRaw, msgID)
		}
		return fmt.Errorf("download/upload failed: %w", err)
	}

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
	// Fallback: attribute नसला तरी MIME / extension वरून detect करा
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

	// ── Thumbnail ──────────────────────────────────────────────────────────
	// Priority 1: User's /setthumbnail bytes
	// Priority 2: Original document's own thumbnail (extracted via userbot)
	var thumbSrc []byte
	if b, ok := getStoredThumbnailBytes(); ok && len(b) > 0 {
		thumbSrc = b
	} else if isVideo {
		// Source doc thumbnail extract करा
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
			// Error visible करा जेणेकरून debug होईल
			editProgress(fmt.Sprintf("⚠️ Thumb upload failed: %v", thumbErr))
		}
	}

	// ── Step 6 + 7 + 8: Send targets decide करा ────────────────────────
	// sendChannel set आहे का?
	if sendChannelPeer != nil {
		// sendChannel set → फक्त sendChannel + log channel
		// Admin ला फक्त progress/error, file नाही

		fileCaption := finalCaption
		if fileCaption == "" {
			fileCaption = file.FileName
		}

		// sendChannel ला send
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

		// Log channel: InputFile consumed झाल्याने doc reference वापरतो
		if markCopied(chatID, msgID) {
			logMedia := tg.InputMediaClass(inputMedia)
			if dm := inputMediaDocFromUpdates(scResult); dm != nil {
				logMedia = dm
			}
			go copyToLogChannelUploaded(tgMsg, logMedia)
		}
	} else {
		// sendChannel नाही → admin ला send करा, नंतर log channel
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

		// Log + source channel: admin send नंतर doc reference वापरतो
		logMedia := tg.InputMediaClass(inputMedia)
		if dm := inputMediaDocFromUpdates(adminResult); dm != nil {
			logMedia = dm
		}

		if markCopied(chatID, msgID) {
			go copyToLogChannelUploaded(tgMsg, logMedia)
		}

		// Source channel असेल तर तिथे पण
		if sourcePeer != nil {
			channelCaption := finalCaption
			if channelCaption == "" {
				channelCaption = file.FileName
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

// ── getChannelPeerIfBotAdmin ──────────────────────────────────────────────────
// Source channel मध्ये bot admin आहे का check करतो.
// Admin असेल तर *tg.InputPeerChannel return करतो, नसेल तर nil.

func getChannelPeerIfBotAdmin(
	ctx *ext.Context,
	botClient *gotgproto.Client,
	channelID int64,
) *tg.InputPeerChannel {
	// channelID is already in internal TG format (negative)
	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, channelID)
	if err != nil {
		// Channel access नाही — bot member नाही
		return nil
	}

	// Bot चा self ID
	if botClient.Self == nil {
		return nil
	}
	botID := botClient.Self.ID

	// Participants check — bot admin आहे का?
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
// io.Writer wrapper — Write होताना progress callback call करतो

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

// progressWriter io.Reader interface सुद्धा implement करतो (upload साठी)
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
