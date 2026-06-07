// internal/commands/admin_stream.go
package commands

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
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

// ── Register ──────────────────────────────────────────────────────────────────

func (m *command) LoadAdminStream(d dispatcher.Dispatcher) {
	log := m.log.Named("admin_stream")
	defer log.Sugar().Info("Loaded /adminstream /userbotdl /setreplace /listreplace /delreplace /clearreplace commands")

	d.AddHandler(handlers.NewCommand("adminstream", adminOnly(cmdAdminStream)))
	d.AddHandler(handlers.NewCommand("userbotdl", adminOnly(cmdUserbotDL)))
	d.AddHandler(handlers.NewCommand("setreplace", adminOnly(cmdSetReplace)))
	d.AddHandler(handlers.NewCommand("listreplace", adminOnly(cmdListReplace)))
	d.AddHandler(handlers.NewCommand("delreplace", adminOnly(cmdDelReplace)))
	d.AddHandler(handlers.NewCommand("clearreplace", adminOnly(cmdClearReplace)))
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
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 120*time.Minute)
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

		sendTextMsg := func(text string) {
			sendCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()
			req := &tg.MessagesSendMessageRequest{
				Peer:     botPeer,
				Message:  text,
				RandomID: rand.Int63(),
			}
			_, _ = botClient.API().MessagesSendMessage(sendCtx, req)
		}

		// ── Process each message in range ─────────────────────────────────
		successCount := 0
		skipCount := 0
		errorCount := 0

		for msgID := startMsg; msgID <= endMsg; msgID++ {

			if bgCtx.Err() != nil {
				sendTextMsg(fmt.Sprintf("⏰ Overall timeout (120min).\n✅ Done: %d | ⏭ Skipped: %d | ❌ Errors: %d", successCount, skipCount, errorCount))
				return
			}

			currentIdx := msgID - startMsg + 1
			editProgress(fmt.Sprintf(
				"⏳ Processing %d/%d\n📡 Channel: `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d",
				currentIdx, totalMsgs, chatIDRaw, msgID,
				successCount, skipCount, errorCount,
			))

			err := processSingleMessage(
				bgCtx,
				userbotClient,
				botClient,
				chatID,
				chatIDRaw,
				msgID,
				topicID,
				botPeer,
				sourcePeerForSend,
				editProgress,
			)

			if err != nil {
				errorCount++
				// Error message admin ला पाठव, पण range continue करा
				sendTextMsg(fmt.Sprintf(
					"❌ Msg `%d` failed:\n`%s`",
					msgID, err.Error(),
				))
				// Range मध्ये थोडा wait करा throttle साठी
				if totalMsgs > 1 {
					time.Sleep(2 * time.Second)
				}
				continue
			}

			successCount++

			// Range मध्ये messages मध्ये थोडा gap द्या Telegram throttle टाळायला
			if totalMsgs > 1 && msgID < endMsg {
				time.Sleep(1 * time.Second)
			}
		}

		// ── Final summary ──────────────────────────────────────────────────
		deleteProgress()

		if totalMsgs == 1 {
			// Single message — progress message आधीच delete केला, काही नाही
			return
		}

		// Range साठी — summary send करा
		sendTextMsg(fmt.Sprintf(
			"✅ *Completed!*\n\n"+
				"📡 Channel: `%d`\n"+
				"📋 Range: `%d` → `%d` (%d msgs)\n\n"+
				"✅ Success: %d\n"+
				"⏭ Skipped (no media/text): %d\n"+
				"❌ Errors: %d",
			chatIDRaw, startMsg, endMsg, totalMsgs,
			successCount, skipCount, errorCount,
		))
	}()

	return dispatcher.EndGroups
}

// ── processSingleMessage ──────────────────────────────────────────────────────
// एका message साठी: fetch → (download+reupload if media) → send to targets
// error return केल्यास caller error count वाढवतो आणि continue करतो

func processSingleMessage(
	bgCtx context.Context,
	userbotClient *gotgproto.Client,
	botClient *gotgproto.Client,
	chatID int64,      // internal TG ID (negative)
	chatIDRaw int64,   // user-facing positive ID (for display)
	msgID int,
	topicID int,
	botPeer tg.InputPeerClass,          // admin chat peer
	sourcePeer *tg.InputPeerChannel,    // source channel peer (nil if bot not admin)
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

		// Source channel मध्ये पण send करा (text)
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
	if doc, ok := tgMsg.Media.(*tg.MessageMediaDocument); ok {
		if d, ok := doc.Document.(*tg.Document); ok {
			for _, attr := range d.Attributes {
				switch attr.(type) {
				case *tg.DocumentAttributeVideo,
					*tg.DocumentAttributeAudio,
					*tg.DocumentAttributeAnimated:
					attributes = append(attributes, attr)
				}
			}
		}
	}

	inputMedia := &tg.InputMediaUploadedDocument{
		File:       uploadedFile,
		MimeType:   file.MimeType,
		Attributes: attributes,
	}

	// Log channel copy (once per chatID+msgID)
	if markCopied(chatID, msgID) {
		go copyToLogChannelUploaded(tgMsg, inputMedia)
	}

	// ── Step 6: Admin ला send ─────────────────────────────────────────────
	adminCaption := fmt.Sprintf(
		"📥 Userbot DL\n📌 Chat: `%d` | Msg: `%d`\n📄 `%s`\n💾 %s\n🔒 Forward protected",
		chatIDRaw, msgID, file.FileName, formatSize(file.FileSize),
	)
	// Caption replace admin message मध्ये पण apply करा
	adminCaption = applyReplaces(adminCaption)

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer sendCancel()

	_, err = botClient.API().MessagesSendMedia(sendCtx, &tg.MessagesSendMediaRequest{
		Peer:       botPeer,
		Media:      inputMedia,
		Message:    adminCaption,
		Noforwards: true,
		RandomID:   rand.Int63(),
	})
	if err != nil {
		return fmt.Errorf("send to admin failed: %w", err)
	}

	// ── Step 7: Source channel मध्ये send (bot admin असेल तर) ────────────
	if sourcePeer != nil {
		// Source channel साठी caption = original caption with replaces (no metadata)
		channelCaption := finalCaption
		if channelCaption == "" {
			channelCaption = file.FileName
		}

		sendCtx2, sendCancel2 := context.WithTimeout(context.Background(), 10*time.Minute)
		defer sendCancel2()

		chReq := &tg.MessagesSendMediaRequest{
			Peer:     sourcePeer,
			Media:    inputMedia,
			Message:  channelCaption,
			RandomID: rand.Int63(),
		}
		// Topic असेल तर reply_to set करा
		if topicID > 0 {
			chReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
		}
		_, _ = botClient.API().MessagesSendMedia(sendCtx2, chReq)
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
