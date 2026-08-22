// internal/commands/admin_stream.go
package commands

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// ── Caption Replace Store ─────────────────────────────────────────────────────
var captionReplaceStore sync.Map // map[string]string

// ── Send Channel Store ────────────────────────────────────────────────────────
var sendChannelStore sync.Map // key: "id", value: int64


func setSendChannelIDForUser(userID int64, channelID int64) {
	sendChannelStore.Store(userID, channelID)
	_ = db.SetUserSendChannel(userID, channelID)
}

func getSendChannelIDForUser(userID int64) (int64, bool) {
	if v, ok := sendChannelStore.Load(userID); ok {
		return v.(int64), true
	}
	if chID, err := db.GetUserSendChannel(userID); err == nil && chID != 0 {
		sendChannelStore.Store(userID, chID)
		return chID, true
	}
	v, ok := sendChannelStore.Load("id")
	if !ok {
		return 0, false
	}
	return v.(int64), true
}

func setWatermarkTextForUser(userID int64, text string) {
	watermarkStore.Store(userID, text)
	_ = db.SetUserWatermark(userID, text)
}

func getWatermarkTextForUser(userID int64) (string, bool) {
	if v, ok := watermarkStore.Load(userID); ok {
		return v.(string), true
	}
	if wm, err := db.GetUserWatermark(userID); err == nil && wm != "" {
		watermarkStore.Store(userID, wm)
		return wm, true
	}
	v, ok := watermarkStore.Load("text")
	if !ok {
		return "", false
	}
	return v.(string), true
}

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

type processInfo struct {
	cancel context.CancelFunc
	userID int64
}

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
// चालू असलेल्या /userbotdl, /predownload, /copychannel processes ला cancel करायला.
// Multiple processes समांतराने (parallel) चालू राहू शकतात.

var (
	activeProcessStore sync.Map // uint64 -> processInfo
	activeProcessSeq   uint64
)

func addActiveProcess(userID int64, cancel context.CancelFunc) uint64 {
	id := atomic.AddUint64(&activeProcessSeq, 1)
	activeProcessStore.Store(id, processInfo{cancel: cancel, userID: userID})
	return id
}

func removeActiveProcess(id uint64) {
	activeProcessStore.Delete(id)
}

func stopAllActiveProcessesForUser(userID int64) int {
	count := 0
	activeProcessStore.Range(func(key, value any) bool {
		if pInfo, ok := value.(processInfo); ok {
			if pInfo.userID == userID && pInfo.cancel != nil {
				pInfo.cancel()
				count++
				activeProcessStore.Delete(key)
			}
		}
		return true
	})
	return count
}

func hasActiveProcessForUser(userID int64) bool {
	hasAny := false
	activeProcessStore.Range(func(key, value any) bool {
		if pInfo, ok := value.(processInfo); ok && pInfo.userID == userID {
			hasAny = true
			return false
		}
		return true
	})
	return hasAny
}

// ── Register ──────────────────────────────────────────────────────────────────

func (m *command) LoadAdminStream(d dispatcher.Dispatcher) {
	log := m.log.Named("admin_stream")
	defer log.Sugar().Info("Loaded /adminstream /userbotdl /stopuserbotdl /copychannel /setreplace /listreplace /delreplace /clearreplace /setsendchannel /showsendchannel /clearsendchannel /setthumbnail /clearthumbnail /showthumbnail /setwatermark /clearwatermark /showwatermark commands")

	d.AddHandler(handlers.NewCommand("adminstream", adminOnly(cmdAdminStream)))
	d.AddHandler(handlers.NewCommand("userbotdl", cmdUserbotDL))
	d.AddHandler(handlers.NewCommand("stopuserbotdl", cmdStopUserbotDL))
	d.AddHandler(handlers.NewCommand("stop", cmdStopUserbotDL))
	d.AddHandler(handlers.NewCommand("copychannel", adminOnly(cmdCopyChannel)))
	d.AddHandler(handlers.NewCommand("predownload", adminOnly(cmdPreDownload)))
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


type tgLinkInfo struct {
	chatIDRaw      int64
	username       string
	topicID        int
	startMsg       int
	endMsg         int
	specificMsgIDs []int
	isLink         bool
}

func parseTelegramPostInput(args []string) (*tgLinkInfo, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no arguments provided")
	}

	first := strings.TrimSpace(args[0])
	if !strings.Contains(first, "t.me/") {
		return nil, nil // Not a URL
	}

	first = strings.Split(first, "?")[0]
	first = strings.Split(first, "#")[0]

	idx := strings.Index(first, "t.me/")
	pathStr := first[idx+len("t.me/"):]
	pathParts := strings.Split(strings.Trim(pathStr, "/"), "/")

	info := &tgLinkInfo{isLink: true}

	if len(pathParts) < 2 {
		return nil, fmt.Errorf("invalid Telegram link format")
	}

	if pathParts[0] == "c" {
		cid, err := strconv.ParseInt(pathParts[1], 10, 64)
		if err != nil || cid <= 0 {
			return nil, fmt.Errorf("invalid channel ID in link")
		}
		info.chatIDRaw = cid

		if len(pathParts) == 3 {
			msgPart := pathParts[2]
			if strings.Contains(msgPart, "-") {
				rangeParts := strings.Split(msgPart, "-")
				s, err1 := strconv.Atoi(rangeParts[0])
				e, err2 := strconv.Atoi(rangeParts[1])
				if err1 == nil && err2 == nil && s > 0 && e >= s {
					info.startMsg = s
					info.endMsg = e
				}
			} else if strings.Contains(msgPart, "/") {
				rangeParts := strings.Split(msgPart, "/")
				s, err1 := strconv.Atoi(rangeParts[0])
				e, err2 := strconv.Atoi(rangeParts[1])
				if err1 == nil && err2 == nil && s > 0 && e >= s {
					info.startMsg = s
					info.endMsg = e
				}
			} else {
				mID, err := strconv.Atoi(msgPart)
				if err == nil && mID > 0 {
					info.startMsg = mID
					info.endMsg = mID
				}
			}
		} else if len(pathParts) >= 4 {
			tID, err1 := strconv.Atoi(pathParts[2])
			mID, err2 := strconv.Atoi(pathParts[3])
			if err1 == nil && tID > 0 {
				info.topicID = tID
			}
			if err2 == nil && mID > 0 {
				info.startMsg = mID
				info.endMsg = mID
			}
		}
	} else {
		info.username = pathParts[0]
		if len(pathParts) >= 2 {
			msgPart := pathParts[1]
			if strings.Contains(msgPart, "-") {
				rangeParts := strings.Split(msgPart, "-")
				s, err1 := strconv.Atoi(rangeParts[0])
				e, err2 := strconv.Atoi(rangeParts[1])
				if err1 == nil && err2 == nil && s > 0 && e >= s {
					info.startMsg = s
					info.endMsg = e
				}
			} else {
				mID, err := strconv.Atoi(msgPart)
				if err == nil && mID > 0 {
					info.startMsg = mID
					info.endMsg = mID
				}
			}
		}
	}

	if len(args) >= 2 {
		second := strings.TrimSpace(args[1])
		if strings.Contains(second, ",") {
			parts := strings.Split(second, ",")
			var ids []int
			for _, p := range parts {
				if id, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && id > 0 {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				info.specificMsgIDs = ids
			}
		} else if endVal, err := strconv.Atoi(second); err == nil && endVal > 0 {
			if info.startMsg > 0 {
				if endVal >= info.startMsg {
					info.endMsg = endVal
				}
			} else {
				info.startMsg = endVal
				info.endMsg = endVal
			}
		}
	}

	return info, nil
}

func cmdGrantUserbotAccess(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: `/grantuserbot <telegram_id_or_mobile_or_username>`\n\nExample: `/grantuserbot 1234567890`")
		return dispatcher.EndGroups
	}
	input := strings.TrimSpace(a[0])

	var tgID int64
	if id, err := strconv.ParseInt(input, 10, 64); err == nil && id > 0 {
		tgID = id
	} else {
		targetUser, uErr := db.GetUserByMobileOrEmail(input)
		if uErr == nil && targetUser != nil && targetUser.TelegramID != 0 {
			tgID = targetUser.TelegramID
		}
	}

	if tgID == 0 {
		reply(ctx, u, fmt.Sprintf("❌ Invalid Telegram ID / User सापडला नाही: `%s`", input))
		return dispatcher.EndGroups
	}

	if err := db.SetUserbotAccess(tgID, true); err != nil {
		reply(ctx, u, "❌ Error setting access: "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("✅ Telegram ID `%d` ला Userbot DL चा Access दिला!", tgID))

	go func() {
		kb := mainReplyKeyboardForUser(tgID)
		if SendTelegramDMWithMarkup != nil {
			_ = SendTelegramDMWithMarkup(tgID, "🎉 *तुम्हाला Userbot Features चा ॲक्सेस मिळाला आहे!*\n\nखालील मेनूमधून तुम्ही युझरबॉट फिचर्स वापरू शकता.", kb)
		}
	}()
	return dispatcher.EndGroups
}


var (
	AdminImpersonateStore sync.Map // adminID (int64) -> targetUserID (int64)
	userbotClientsMu      sync.Mutex
	userbotClients        = make(map[int64]*gotgproto.Client)
)

func GetUserbotClientForUser(userID int64) (*gotgproto.Client, error) {
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, userID)
	if isAdmin {
		if targetUser, ok := AdminImpersonateStore.Load(userID); ok {
			userID = targetUser.(int64)
		} else {
			if GetLiveUserbotClient != nil {
				if liveClient := GetLiveUserbotClient(); liveClient != nil {
					return liveClient, nil
				}
			}
			if GetUserbotClient != nil {
				if uc := GetUserbotClient(); uc != nil {
					return uc, nil
				}
			}
		}
	}

	userbotClientsMu.Lock()
	defer userbotClientsMu.Unlock()

	if client, ok := userbotClients[userID]; ok && client != nil {
		return client, nil
	}

	doc, err := db.GetUserBotSessionDocForUser(userID)
	if err != nil || doc == nil || doc.Session == "" {
		return nil, fmt.Errorf("Userbot account not connected. Please login first.")
	}

	client, err := gotgproto.NewClient(
		int(config.ValueOf.ApiID),
		config.ValueOf.ApiHash,
		gotgproto.ClientTypePhone(""),
		&gotgproto.ClientOpts{
			Session:          sessionMaker.PyrogramSession(doc.Session),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("Failed to start your userbot session: %w", err)
	}

	userbotClients[userID] = client
	return client, nil
}

func resolveChannelInputPeer(ctx context.Context, client *gotgproto.Client, channelID int64) *tg.InputPeerChannel {
	if client == nil || channelID == 0 {
		return nil
	}
	if peer, err := utils.GetChannelPeer(ctx, client.API(), client.PeerStorage, channelID); err == nil && peer != nil {
		return &tg.InputPeerChannel{
			ChannelID:  peer.ChannelID,
			AccessHash: peer.AccessHash,
		}
	}

	mtprotoID := channelID
	if mtprotoID < 0 {
		mtprotoID = (-mtprotoID) - 1000000000000
	}

	cCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	chats, err := client.API().ChannelsGetChannels(cCtx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: mtprotoID},
	})
	if err == nil && len(chats.GetChats()) > 0 {
		if ch, ok := chats.GetChats()[0].(*tg.Channel); ok {
			return &tg.InputPeerChannel{
				ChannelID:  ch.ID,
				AccessHash: ch.AccessHash,
			}
		}
	}
	return nil
}

// ── /stopuserbotdl ────────────────────────────────────────────────────────────
// सध्या चालू असलेल्या सगळ्या background processes cancel करतो.

func cmdStopUserbotDL(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	if !hasActiveProcessForUser(senderID) {
		reply(ctx, u, "⚠️ सध्या तुमचा कोणताही process चालू नाही.")
		return dispatcher.EndGroups
	}
	stopped := stopAllActiveProcessesForUser(senderID)
	if stopped > 0 {
		reply(ctx, u, fmt.Sprintf("🛑 एकूण %d चालू batch processes stop केले!\n\nजे messages आधीच send झाले ते cancel होणार नाहीत.\nSummary लवकरच update होईल.", stopped))
	} else {
		reply(ctx, u, "⚠️ Process आधीच थांबले होते.")
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
	senderID := u.EffectiveUser().GetID()
	cleared := false

	if hasWatermark() {
		watermarkStore.Delete("text")
		cleared = true
	}

	if _, ok := watermarkStore.Load(senderID); ok {
		watermarkStore.Delete(senderID)
		cleared = true
	}
	if wm, err := db.GetUserWatermark(senderID); err == nil && wm != "" {
		_ = db.SetUserWatermark(senderID, "")
		cleared = true
	}

	if !cleared {
		reply(ctx, u, "💧 कोणताही watermark set नाही.")
		return dispatcher.EndGroups
	}

	reply(ctx, u, "✅ Watermark clear केला. Video uploads आता watermark शिवाय जातील.")
	return dispatcher.EndGroups
}

// ── /showwatermark ────────────────────────────────────────────────────────────

func cmdShowWatermark(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	text, ok := getWatermarkTextForUser(senderID)
	if !ok || text == "" {
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
	text := u.EffectiveMessage.Text
	
	// Command आणि args वेगळे करणे
	partsCmd := strings.SplitN(text, " ", 2)
	if len(partsCmd) < 2 || strings.TrimSpace(partsCmd[1]) == "" {
		reply(ctx, u,
			"📝 *Usage:* `/setreplace old_text=new_text`\n\n"+
				"Examples:\n"+
				"`/setreplace hello=नमस्ते`\n"+
				"`/setreplace Please subscribe to my channel.= ` (काढून टाकण्यासाठी)\n\n"+
				"Caption मधील 'old_text' आढळल्यास 'new_text' ने replace होईल.\n"+
				"सध्याचे rules बघायला: /listreplace")
		return dispatcher.EndGroups
	}

	// पुढे multiple spaces असल्यास फक्त सुरुवातीचे spaces काढायचे, मधले तसेच ठेवायचे.
	fullText := strings.TrimLeft(partsCmd[1], " \t")
	parts := strings.SplitN(fullText, "=", 2)
	if len(parts) != 2 {
		reply(ctx, u, "❌ Invalid format. '=' असणे आवश्यक आहे.\nExample: `/setreplace old=new`")
		return dispatcher.EndGroups
	}

	// oldWord मध्ये जेवढे spaces असतील (सुरुवात/शेवट सोडून) ते तसेच राहतील.
	oldWord := parts[0]
	newWord := parts[1]
	
	// जर user ने quotes दिले असतील, तर आपण trimming करू शकतो.
	// पण तूर्तास सुरळीतपणासाठी आपण फक्त trailing spaces काढूया.
	// TrimSpace केल्यास oldWord च्या सुरुवातीचे/शेवटचे spaces जातील. जर त्याला तिथे spaces हवे असतील तर?
	// सहसा ते कॉपी-पेस्ट मध्ये येतात.
	oldWord = strings.TrimSpace(oldWord)
	newWord = strings.TrimSpace(newWord)
	
	// If the user deliberately provides quotes for exact matching, we can remove them.
	// But TrimSpace usually handles normal use cases.
	
	captionReplaceStore.Store(oldWord, newWord)

	reply(ctx, u, fmt.Sprintf("✅ Replace rule set:\n\n`%s`\n⬇️\n`%s`", oldWord, newWord))
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
	senderID := u.EffectiveUser().GetID()
	var targetUserID int64 = senderID
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	if isAdmin && len(a) >= 2 && a[0] == "-u" {
		if uid, err := strconv.ParseInt(a[1], 10, 64); err == nil {
			targetUserID = uid
			a = a[2:]
		}
	}

	if len(a) < 1 {
		curID, ok := getSendChannelIDForUser(targetUserID)
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

	setSendChannelIDForUser(targetUserID, internalID)

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
		oldStr := k.(string)
		newStr := v.(string)
		
		// Case-insensitive and whitespace-flexible regex replacement
		escapedOld := regexp.QuoteMeta(oldStr)
		// Any sequence of whitespace in the user's search string will match ANY sequence of whitespace in the caption
		escapedOld = regexp.MustCompile(`\s+`).ReplaceAllString(escapedOld, `\s+`)
		
		re, err := regexp.Compile("(?i)" + escapedOld)
		if err == nil {
			caption = re.ReplaceAllString(caption, newStr)
		} else {
			// Fallback if regex fails
			caption = strings.ReplaceAll(caption, oldStr, newStr)
		}
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
	senderID := u.EffectiveUser().GetID()
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	if !isAdmin {
		if !db.HasUserbotAccess(senderID) {
			reply(ctx, u, "⛔ तुम्हाला Userbot DL चा ॲक्सेस नाही. Admin ला सांगा access द्यायला.")
			return dispatcher.EndGroups
		}
		doc, _ := db.GetUserBotSessionDocForUser(senderID)
		if doc == nil || doc.Session == "" {
			ctx.Reply(u, ext.ReplyTextString("❌ *Userbot Account Not Connected!*\n\nUserbot DL वापरण्यासाठी आधी तुमचे Account Login करा."), &ext.ReplyOpts{
				Markup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
					{Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "🔑 Connect Userbot Account", Data: []byte("wc:userbot_login")},
					}},
				}},
			})
			return dispatcher.EndGroups
		}
	}

	a := args(u)
	var targetUserID int64 = senderID

	if isAdmin && len(a) >= 2 && a[0] == "-u" {
		if uid, err := strconv.ParseInt(a[1], 10, 64); err == nil {
			targetUserID = uid
			a = a[2:]
		}
	}

	if len(a) < 1 {
		reply(ctx, u,
			"📥 *Usage:*\n\n"+
				"`/userbotdl <chatID> <msgID>` OR `/userbotdl <chatID> <start> <end>`\n\n"+
				"*Post Link Examples:*\n"+
				"`https://t.me/c/4372880940/1687` (Single Post Link)\n"+
				"`https://t.me/c/4372880940/1687 1700` (Link + End Range)\n"+
				"`https://t.me/c/4372880940/1687-1700` (Range Link)\n\n"+
				"Stop करायला: /stopuserbotdl")
		return dispatcher.EndGroups
	}

	var chatIDRaw int64
	var chatID int64
	var topicID int
	var startMsg int
	var endMsg int
	var specificMsgIDs []int

	
	if isAdmin {
		GlobalDashboard.SetAdminChat(os.Getenv("BOT_TOKEN"), u.EffectiveChat().GetID())
	}

	linkInfo, linkErr := parseTelegramPostInput(a)
	if linkErr != nil {
		reply(ctx, u, "❌ "+linkErr.Error())
		return dispatcher.EndGroups
	}

	if linkInfo != nil && linkInfo.isLink {
		if linkInfo.chatIDRaw != 0 {
			chatIDRaw = linkInfo.chatIDRaw
			if chatIDRaw > 0 {
				chatID = -1000000000000 - chatIDRaw
			} else {
				chatID = chatIDRaw
			}
		}
		topicID = linkInfo.topicID
		specificMsgIDs = linkInfo.specificMsgIDs
		startMsg = linkInfo.startMsg
		endMsg = linkInfo.endMsg
	} else {
		if len(a) < 2 {
			reply(ctx, u, "❌ Invalid arguments. ChatID आणि MsgID/Range टाका किंवा Post Link वापरा.")
			return dispatcher.EndGroups
		}
		raw, err := strconv.ParseInt(a[0], 10, 64)
		if err != nil || raw == 0 {
			reply(ctx, u, "❌ Invalid chatID.\nt.me/c/ वरील positive ID वापरा.")
			return dispatcher.EndGroups
		}
		chatIDRaw = raw
		if chatIDRaw > 0 {
			chatID = -1000000000000 - chatIDRaw
		} else {
			chatID = chatIDRaw
		}

		parseCommaMsgIDs := func(s string) ([]int, bool) {
			parts := strings.Split(s, ",")
			ids := make([]int, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				id, err := strconv.Atoi(p)
				if err != nil || id < 1 {
					return nil, false
				}
				ids = append(ids, id)
			}
			return ids, len(ids) > 0
		}

		switch len(a) {
		case 2:
			if strings.Contains(a[1], ",") {
				ids, ok := parseCommaMsgIDs(a[1])
				if !ok {
					reply(ctx, u, "❌ Invalid message IDs.")
					return dispatcher.EndGroups
				}
				specificMsgIDs = ids
			} else {
				msgID, err := strconv.Atoi(a[1])
				if err != nil || msgID < 1 {
					reply(ctx, u, "❌ Invalid messageID.")
					return dispatcher.EndGroups
				}
				startMsg = msgID
				endMsg = msgID
			}

		case 3:
			if strings.Contains(a[2], ",") {
				t, err1 := strconv.Atoi(a[1])
				if err1 != nil || t < 1 {
					reply(ctx, u, "❌ Invalid topicID.")
					return dispatcher.EndGroups
				}
				ids, ok := parseCommaMsgIDs(a[2])
				if !ok {
					reply(ctx, u, "❌ Invalid message IDs.")
					return dispatcher.EndGroups
				}
				topicID = t
				specificMsgIDs = ids
			} else {
				s, err1 := strconv.Atoi(a[1])
				e, err2 := strconv.Atoi(a[2])
				if err1 != nil || err2 != nil || s < 1 || e < 1 || s > e {
					reply(ctx, u, "❌ Invalid range.")
					return dispatcher.EndGroups
				}
				startMsg = s
				endMsg = e
			}

		case 4:
			t, err1 := strconv.Atoi(a[1])
			s, err2 := strconv.Atoi(a[2])
			e, err3 := strconv.Atoi(a[3])
			if err1 != nil || err2 != nil || err3 != nil || t < 1 || s < 1 || e < 1 || s > e {
				reply(ctx, u, "❌ Invalid range / topicID.")
				return dispatcher.EndGroups
			}
			topicID = t
			startMsg = s
			endMsg = e
		}
	}

	userbotClient, err := GetUserbotClientForUser(targetUserID)
	if err != nil || userbotClient == nil {
		reply(ctx, u, "❌ Userbot Client ready नाही. आधी 🔑 Connect Userbot Account वरून Login करा.")
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	// आधी एखादा process चालू असेल तर warn करा

	adminChatID := u.EffectiveChat().GetID()
	botPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)

	sourcePeerForSend := getChannelPeerIfBotAdmin(ctx, botClient, chatID)

	var sendChannelPeer *tg.InputPeerChannel
	if scID, ok := getSendChannelIDForUser(targetUserID); ok && scID != 0 {
		sendChannelPeer = resolveChannelInputPeer(ctx, botClient, scID)
		if sendChannelPeer == nil && userbotClient != nil {
			sendChannelPeer = resolveChannelInputPeer(ctx, userbotClient, scID)
		}
	}

	totalMsgs := 0
	rangeStr := ""
	if len(specificMsgIDs) > 0 {
		totalMsgs = len(specificMsgIDs)
		idStrsPreview := make([]string, len(specificMsgIDs))
		for i, id := range specificMsgIDs {
			idStrsPreview[i] = fmt.Sprintf("`%d`", id)
		}
		rangeStr = fmt.Sprintf("Msgs: %s (%d total)", strings.Join(idStrsPreview, ", "), totalMsgs)
	} else {
		totalMsgs = endMsg - startMsg + 1
		if startMsg == endMsg {
			rangeStr = fmt.Sprintf("Msg `%d`", startMsg)
		} else {
			rangeStr = fmt.Sprintf("Msgs `%d` → `%d` (%d total)", startMsg, endMsg, totalMsgs)
		}
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
	if wmText, ok := getWatermarkTextForUser(senderID); ok {
		watermarkStr = fmt.Sprintf("\n💧 Watermark: `%s`", wmText)
	}

		sentMsgID := 0
	if !isAdmin {
		sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
			"⏳ Starting...\n📡 Channel: %d\n📑 %s%s%s%s\n\n🛑 Stop: /stopuserbotdl",
			chatIDRaw, rangeStr, topicStr, channelStr, watermarkStr,
		)), nil)
		if sentMsg != nil {
			sentMsgID = sentMsg.ID
		}
	}

	// ── Goroutine: process all messages ──────────────────────────────────────
	go func() {
		// [NEW] Cancel context — /stopuserbotdl ने cancel करता येतो
		bgCtx, bgCancel := context.WithCancel(context.Background())
		pID := addActiveProcess(senderID, bgCancel)
		defer func() {
			bgCancel()
			removeActiveProcess(pID)
		}()

		var editMu sync.Mutex
		var lastEdit time.Time
		var editCount int
		var msgCreatedAt = time.Now()

		editProgress := func(text string) {
			editMu.Lock()
			if sentMsgID == 0 {
				editMu.Unlock()
				return
			}
			
		isFinalMsg := strings.HasPrefix(text, "❌") || strings.HasPrefix(text, "✅")
			if isFinalMsg {

			editMu.Lock()
			currentMsgID := sentMsgID
			editMu.Unlock()
			if currentMsgID != 0 {
				botToken := os.Getenv("BOT_TOKEN")
				wmqueue.DeleteTgMessage(botToken, u.EffectiveChat().GetID(), int64(currentMsgID))
				newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
				if newID != 0 {
					editMu.Lock()
					sentMsgID = int(newID)
					editMu.Unlock()
				}
			}
			return
		}
// 5s frequency as requested by user
			if time.Since(lastEdit) < 5*time.Second {
				editMu.Unlock()
				return
			}
			
			botToken := os.Getenv("BOT_TOKEN")
			needsNewMsg := false
			if editCount >= 10 || time.Since(msgCreatedAt) >= 2*time.Minute {
				needsNewMsg = true
			} else if _, waiting := wmqueue.InEditFloodWait(botToken); waiting {
				needsNewMsg = true
				wmqueue.ClearEditFloodWait(botToken)
			}
			
			lastEdit = time.Now()
			currentMsgID := sentMsgID
			editMu.Unlock()

			go func(msgID int) {
				if needsNewMsg {
					newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
					if newID != 0 {
						editMu.Lock()
						sentMsgID = int(newID)
						editCount = 0
						msgCreatedAt = time.Now()
						editMu.Unlock()
						wmqueue.DeleteTgMessage(botToken, adminChatID, int64(msgID))
					}
					return
				}

				// Otherwise edit
				success := wmqueue.EditTgMessage(botToken, adminChatID, msgID, text)
				if !success {
					// Edit failed (possibly FloodWait), delete and send new
					newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
					if newID != 0 {
						editMu.Lock()
						sentMsgID = int(newID)
						editCount = 0
						msgCreatedAt = time.Now()
						editMu.Unlock()
						wmqueue.DeleteTgMessage(botToken, adminChatID, int64(msgID))
					}
				} else {
					editMu.Lock()
					editCount++
					editMu.Unlock()
				}
			}(currentMsgID)
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

		var currentProcessingMsgID int // Added to track current MsgID

		buildSummaryText := func() string {
			statusLine := ""
			if stopped {
				statusLine = "\n🛑 *Process stopped*"
			}
			var s string
			if len(specificMsgIDs) > 0 {
				idStrs := make([]string, len(specificMsgIDs))
				for i, id := range specificMsgIDs {
					idStrs[i] = fmt.Sprintf("`%d`", id)
				}
				s = fmt.Sprintf(
					"📊 *Summary*\n📡 `%d` | Msgs: %s\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n🔄 Current MsgID: `%d`%s",
					chatIDRaw, strings.Join(idStrs, ", "),
					successCount, skipCount, errorCount, currentProcessingMsgID, statusLine,
				)
			} else {
				s = fmt.Sprintf(
					"📊 *Summary*\n📡 `%d` | Range: `%d`→`%d`\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n🔄 Current MsgID: `%d`%s",
					chatIDRaw, startMsg, endMsg,
					successCount, skipCount, errorCount, currentProcessingMsgID, statusLine,
				)
			}
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

		var summaryEditMu sync.Mutex
		var summaryEditCount int
		var summaryCreatedAt = time.Now()

		updateSummary := func() {
			botToken := os.Getenv("BOT_TOKEN")
			text := buildSummaryText()
			
			summaryEditMu.Lock()
			if summaryMsgID == 0 {
				summaryEditMu.Unlock()
				newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
				}
				return
			}

			needsNewMsg := false
			if summaryEditCount >= 10 || time.Since(summaryCreatedAt) >= 2*time.Minute {
				needsNewMsg = true
			} else if _, waiting := wmqueue.InEditFloodWait(botToken); waiting {
				needsNewMsg = true
				wmqueue.ClearEditFloodWait(botToken)
			}
			currentSummaryID := summaryMsgID
			summaryEditMu.Unlock()

			if needsNewMsg {
				newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
					wmqueue.DeleteTgMessage(botToken, adminChatID, int64(currentSummaryID))
				}
				return
			}

			success := wmqueue.EditTgMessage(botToken, adminChatID, currentSummaryID, text)
			if !success {
				newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
					wmqueue.DeleteTgMessage(botToken, adminChatID, int64(currentSummaryID))
				}
			} else {
				summaryEditMu.Lock()
				summaryEditCount++
				summaryEditMu.Unlock()
			}
		}

		// ── runPhaseA — retries आणि FloodWait चातुर्याने हाताळत Phase A (prepareMessage) चालवतो ──
		runPhaseA := func(msgID int) (*preparedMessage, error) {
			const maxRetries = 3
			var pm *preparedMessage
			var err error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if bgCtx.Err() != nil {
					return nil, err
				}
				pm, err = prepareMessage(
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
					adminChatID,
					&sentMsgID,
				)
				if err == nil {
					return pm, nil
				}
				if bgCtx.Err() != nil {
					return nil, err
				}
				if waitSecs, isFlood := extractFloodWaitSeconds(err); isFlood {
					editProgress(fmt.Sprintf("⏳ FloodWait %ds — थांबतोय...\n📌 Msg: `%d`", waitSecs, msgID))
					select {
					case <-time.After(time.Duration(waitSecs+2) * time.Second):
					case <-bgCtx.Done():
						return nil, bgCtx.Err()
					}
				} else if strings.Contains(strings.ToLower(err.Error()), "deleted") {
					return nil, err
				} else if attempt < maxRetries {
					editProgress(fmt.Sprintf("🔄 Retry %d/%d\n📌 Msg: `%d`", attempt, maxRetries, msgID))
					time.Sleep(5 * time.Second)
				}
			}
			return nil, err
		}

		// ── Pipeline: Phase A(msgID+1) आणि Phase B(msgID) parallel ────────────
		// lookahead=1 — एका वेळी जास्तीत जास्त 2 messages in-flight (Phase A
		// साठी current msgID चं Phase A आधीच झालेलं असतं, पुढच्याचं prefetch चालू).
		// Colab कडे यामुळे पुढचा job लगेच submit होतो — Koyeb चं Telegram upload
		// (Phase B) चालू असतानाच Colab download+ffmpeg सुरू करू शकतो.

		type phaseAOutcome struct {
			pm  *preparedMessage
			err error
		}

		// सध्याच्या msgID चं Phase A आधीच झालेलं असेल तर तो result इथे ठेवतो
		var pendingPhaseA *phaseAOutcome
		var pendingMsgID int

		// ── message IDs list तयार करो ─────────────────────────────────────────
		var msgIDList []int
		if len(specificMsgIDs) > 0 {
			msgIDList = specificMsgIDs
		} else {
			msgIDList = make([]int, 0, endMsg-startMsg+1)
			for id := startMsg; id <= endMsg; id++ {
				msgIDList = append(msgIDList, id)
			}
		}

		for msgIdx, msgID := range msgIDList {

			// Stop check
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			currentIdx := msgIdx + 1
			currentProcessingMsgID = msgID
			editProgress(fmt.Sprintf(
				"⏳ Processing %d/%d\n📡 Channel: `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n\n🛑 Stop: /stopuserbotdl",
				currentIdx, totalMsgs, chatIDRaw, msgID,
				successCount, skipCount, errorCount,
			))
			updateSummary()

			// ── Phase A for msgID — आधीच prefetch झालं असेल तर वापरो, नाहीतर आता चालवो ──
			var phA phaseAOutcome
			if pendingPhaseA != nil && pendingMsgID == msgID {
				phA = *pendingPhaseA
				pendingPhaseA = nil
			} else {
				pm, err := runPhaseA(msgID)
				phA = phaseAOutcome{pm: pm, err: err}
			}

			// Stop झाला असेल तर
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			if phA.err != nil {
				errorCount++
				errKey := phA.err.Error()
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

			// ── Phase B(msgID) goroutine मध्ये सुरू करो — Telegram upload/send ──
			var phaseBErr error
			var phaseBWg sync.WaitGroup
			phaseBWg.Add(1)
			go func(pm *preparedMessage) {
				defer phaseBWg.Done()
				phaseBErr = sendPreparedMessage(
					bgCtx,
					userbotClient,
					botClient,
					chatID,
					chatIDRaw,
					topicID,
					botPeer,
					sourcePeerForSend,
					sendChannelPeer,
					editProgress,
					pm,
				)
			}(phA.pm)

			// ── त्याच वेळी पुढच्या msgID चं Phase A prefetch करो (lookahead=1) ──
			if msgIdx+1 < len(msgIDList) && bgCtx.Err() == nil {
				nextMsgID := msgIDList[msgIdx+1]
				pm, err := runPhaseA(nextMsgID)
				pendingPhaseA = &phaseAOutcome{pm: pm, err: err}
				pendingMsgID = nextMsgID
			}

			// ── Phase B(msgID) पूर्ण होण्याची वाट ──────────────────────────────
			phaseBWg.Wait()

			// Stop झाला असेल तर
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			if phaseBErr != nil {
				errorCount++
				errKey := phaseBErr.Error()
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

			if totalMsgs > 1 && msgIdx < len(msgIDList)-1 {
				time.Sleep(3 * time.Second)
				if successCount > 0 && successCount%10 == 0 {
					editProgress("⏳ Anti-Spam Delay: Telegram limits टाळण्यासाठी 15 सेकंद थांबतोय...")
					time.Sleep(15 * time.Second)
				}
			}
		}

		deleteProgress()
		updateSummary()
	}()

	return dispatcher.EndGroups
}

// ── extractFloodWaitSeconds ────────────────────────────────────────────────────
// [FIX] gotd ची actual error string "FLOOD_WAIT (7067)" अशी असते (underscore
// नाही, space + parentheses) — जुना regex (`FLOOD_WAIT_(\d+)`) फक्त
// "FLOOD_WAIT_123" पॅटर्न शोधत होता, त्यामुळे प्रत्यक्ष error कधीच match होत
// नव्हता आणि flood-wait कधीच detect होत नव्हता (नुसता generic warn+rotate
// होत राहायचा, न थांबता). आता FLOOD_WAIT नंतर मधे काहीही (space/paren/
// underscore) असो, पुढचा आकडा पकडतो.
var floodWaitRe = regexp.MustCompile(`FLOOD_WAIT\D*(\d+)`)

func extractFloodWaitSeconds(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := floodWaitRe.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return 0, false
	}
	secs, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return secs, true
}

// ── runWithFloodRetry ─────────────────────────────────────────────────────────
// [NEW] कुठलंही Telegram API call (fn) FLOOD_WAIT आल्यास आवश्यक तितका वेळ
// थांबून आपोआप retry करतो (जास्तीत जास्त maxFloodRetries वेळा — सलग खूप जास्त
// FloodWait आल्यास infinite loop टाळायला cap). FLOOD_WAIT नसलेला error आला तर
// लगेच तो error परत देतो (retry करत नाही — तो caller च्या fallback logic ने
// हाताळायचा). transport-level middleware (GetFloodMiddleware) आधीच काही
// FLOOD_WAIT स्वतः handle करतं, पण bulk copy मध्ये मोठे/वारंवार waits त्याच्या
// max-retry (10) च्या पलीकडे जाऊ शकतात — म्हणून इथे command-level वर सुद्धा
// एक जास्तीचा safety-net ठेवलाय.
func runWithFloodRetry(bgCtx context.Context, editProgress func(string), msgID int, maxFloodRetries int, fn func() error) error {
	var err error
	for attempt := 0; attempt <= maxFloodRetries; attempt++ {
		if bgCtx.Err() != nil {
			return bgCtx.Err()
		}
		err = fn()
		if err == nil {
			return nil
		}
		waitSecs, isFlood := extractFloodWaitSeconds(err)
		if !isFlood {
			return err // FloodWait नाही — इथेच थांबतो, caller fallback ठरवेल
		}
		if attempt == maxFloodRetries {
			return err // कितीही थांबलो तरी FloodWait संपतच नाहीये — सोडून देतो
		}
		editProgress(fmt.Sprintf("⏳ FloodWait %ds — थांबतोय...\n📌 Msg: `%d`", waitSecs, msgID))
		select {
		case <-time.After(time.Duration(waitSecs+2) * time.Second):
		case <-bgCtx.Done():
			return bgCtx.Err()
		}
	}
	return err
}

// ── fastCopyMessage ───────────────────────────────────────────────────────────
// [NEW] Watermark नसताना वापरायचा जलद मार्ग — कुठलाही download/reupload न
// करता, message चा existing file reference (ID+AccessHash+FileReference)
// वापरून थेट destination ला नवीन message म्हणून पाठवतो (server-side reference
// copy). हेच catalog.go च्या "watermark disabled/skip" flow मध्ये वापरलं
// जातं — bot bandwidth/वेळ अजिबात लागत नाही, आणि "restrict saving content"
// लाही touch करत नाही (हे forward/copy_message नाही, brand-new message आहे,
// फक्त तोच underlying file object वापरून).
//
// [FIX] फक्त botClient वापरतो — userbot अजिबात लागत नाही. Bot आधीच source
// channel चा admin असल्यामुळे (catalog.go च्या handlePlayLecture प्रमाणेच)
// ChannelsGetMessages ने थेट bot कडून message/media मिळवता येतो. Userbot फक्त
// तेव्हाच लागेल जेव्हा bot admin नसून userbot ला access आहे — तो केस
// cmdCopyChannel च्या fallback (जड download) मार्गात हाताळला जातो, इथे नाही.
func fastCopyMessage(
	bgCtx context.Context,
	botClient *gotgproto.Client,
	sourceChatID int64,
	msgID int,
	destPeer *tg.InputPeerChannel,
) error {
	sourceChannel, err := utils.GetChannelPeer(bgCtx, botClient.API(), botClient.PeerStorage, sourceChatID)
	if err != nil {
		return fmt.Errorf("source channel access error: %w", err)
	}
	res, err := botClient.API().ChannelsGetMessages(bgCtx, &tg.ChannelsGetMessagesRequest{
		Channel: sourceChannel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return fmt.Errorf("message fetch failed: %w", err)
	}
	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		return fmt.Errorf("message not found (msg %d)", msgID)
	}
	tgMsg, ok := messages.Messages[0].(*tg.Message)
	if !ok {
		return fmt.Errorf("message deleted/inaccessible (msg %d)", msgID)
	}
	finalCaption := applyReplaces(strings.TrimSpace(tgMsg.Message))

	sendCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	if tgMsg.Media == nil {
		_, err := botClient.API().MessagesSendMessage(sendCtx, &tg.MessagesSendMessageRequest{
			Peer: destPeer, Message: finalCaption, RandomID: rand.Int63(),
		})
		return err
	}

	var inputMedia tg.InputMediaClass
	switch m := tgMsg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return fmt.Errorf("document read error (msg %d)", msgID)
		}
		inputMedia = &tg.InputMediaDocument{
			ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference},
		}
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return fmt.Errorf("photo read error (msg %d)", msgID)
		}
		inputMedia = &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference},
		}
	default:
		return fmt.Errorf("unsupported media type (msg %d)", msgID)
	}

	_, err = botClient.API().MessagesSendMedia(sendCtx, &tg.MessagesSendMediaRequest{
		Peer: destPeer, Media: inputMedia, Message: finalCaption, RandomID: rand.Int63(),
	})
	return err
}

// ══════════════════════════════════════════════════════════════════════════════
// /predownload — Main Command
// ══════════════════════════════════════════════════════════════════════════════
// [NEW] Admin ला server वर load नसताना (रात्री/कमी traffic वेळी) एका specific
// user साठी संपूर्ण course चे सगळे lectures आधीच process करून ठेवायची सोय —
// त्या user च्याच watermark (mobile/email) सोबत. हेच handlePlayLecture च्या
// core engine (deliverLectureCore) वापरतं — त्यामुळे प्रत्येक lecture साठी
// video त्या user ला लगेच पाठवला जातो, आणि LOG_CHANNEL मध्ये backup राहतो
// (DeliveredCache मध्ये save होऊन). त्यामुळे तो user स्वतःहून bot मध्ये तीच
// lecture परत मागेल तेव्हा (delete केली असली तरी) — fast-path (cache-hit)
// मुळे लगेच log-channel वरून forward होतं, नवीन watermark job चालतच नाही.
func cmdPreDownload(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"📥 *Usage:*\n\n"+
				"`/predownload <mobile_or_email_or_telegramID> <courseID>`\n\n"+
				"*Example:*\n"+
				"`/predownload 9876543210 64f1a2b3c4d5e6f7a8b9c0d1`\n\n"+
				"⚠️ Target user चा Telegram account आधीच link (registered) असायला हवा.\n"+
				"⚠️ प्रत्येक lecture त्या user ला त्याच्याच watermark सोबत लगेच पाठवली जाईल आणि पुढच्या fast-access साठी cache होईल.\n"+
				"Stop करायला: /stopuserbotdl")
		return dispatcher.EndGroups
	}

	identifier := a[0]
	courseID, cErr := primitive.ObjectIDFromHex(a[1])
	if cErr != nil {
		reply(ctx, u, "❌ Invalid courseID.")
		return dispatcher.EndGroups
	}

	targetUser, uErr := db.GetUserByMobileOrEmail(identifier)
	if uErr != nil || targetUser == nil {
		if tgID, pErr := strconv.ParseInt(identifier, 10, 64); pErr == nil {
			targetUser, uErr = db.GetUserByTelegramID(tgID)
		}
	}
	if uErr != nil || targetUser == nil {
		reply(ctx, u, fmt.Sprintf("❌ User सापडला नाही: `%s` (mobile/email/telegramID तपासून बघ)", identifier))
		return dispatcher.EndGroups
	}
	if targetUser.TelegramID == 0 {
		reply(ctx, u, "❌ या user चं Telegram account अजून link नाहीये — त्याने आधी `/register` करायला हवं.")
		return dispatcher.EndGroups
	}
	hasAccess, _ := db.HasAccess(targetUser.ID, courseID)
	if !hasAccess {
		reply(ctx, u, "❌ या user कडे या course चा access नाहीये.")
		return dispatcher.EndGroups
	}
	course, courseErr := db.GetCourseByID(courseID)
	if courseErr != nil || course == nil {
		reply(ctx, u, "❌ Course सापडला नाही.")
		return dispatcher.EndGroups
	}

	files, fErr := db.FilesForCourse(courseID)
	if fErr != nil {
		reply(ctx, u, "❌ Error: "+fErr.Error())
		return dispatcher.EndGroups
	}
	if len(files) == 0 {
		reply(ctx, u, "ℹ️ या course मध्ये काहीच lectures नाहीत.")
		return dispatcher.EndGroups
	}
	msgIDs := make([]int, 0, len(files))
	for _, f := range files {
		msgIDs = append(msgIDs, f.MessageID)
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}


	total := len(msgIDs)
	adminChatID := u.EffectiveChat().GetID()
	adminPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
		"⏳ Pre-download सुरू...\n👤 User: `%s` (mobile: `%s`)\n📚 Course: `%s`\n📦 एकूण: %d lectures\n\n🛑 Stop: /stopuserbotdl",
		targetUser.FullName, targetUser.Mobile, course.Title, total,
	)), nil)
	statusMsgID := 0
	if sentMsg != nil {
		statusMsgID = sentMsg.ID
	}

	go func() {
		bgCtx, bgCancel := context.WithCancel(context.Background())
		pID := addActiveProcess(senderID, bgCancel)
		defer func() {
			bgCancel()
			removeActiveProcess(pID)
		}()

		var lastEdit time.Time
		var msgCreatedAt = time.Now() // statusMsgID कधी (नवीन) पाठवला गेला
		var floodUntil time.Time      // [NEW] याच्या आधी कुठलाही edit/send/delete call करायचा नाही
		var editMu sync.Mutex

		// [NEW] एकाच message वर सतत खूप edits केल्यास (किंवा edits खूप जवळून
		// पाठवल्यास) Telegram FLOOD_WAIT देतं. त्यामुळे status message दर
		// 10 मिनिटांनी delete करून नवीन पाठवतो — म्हणजे कुठल्याही एका
		// message वरचा edit-count मर्यादित राहतो आणि flood wait टळतो.
		const statusMsgMaxAge = 10 * time.Minute

		// [FIX] FLOOD_WAIT चा actual कालावधी काही सेकंद ते तासभर (उदा. आपल्या
		// logs मध्ये दिसलेला ~7067s ≈ जवळपास 2 तास) इतका मोठा असू शकतो.
		// त्यामुळे तो synchronously "थांबून मग retry" करणं चुकीचं — दर 6
		// सेकंदाला परत तेच FLOOD_WAIT मिळत राहतं (कारण खरा wait अजून संपलेलाच
		// नसतो) आणि प्रत्येक वेळी एक नवीन notification + नवीन blocking-wait
		// तयार होत राहतो (spam). त्याऐवजी: एकदा FLOOD_WAIT आला की तेवढा वेळ
		// संपेपर्यंत (floodUntil) कुठलाही edit/send/delete call करायचाच नाही —
		// फक्त सुरुवातीला एकदाच notify करायचं, बाकी वेळ शांतपणे skip करायचं.
		markFlood := func(waitSecs int, context_ string) bool {
			editMu.Lock()
			alreadyKnown := time.Now().Before(floodUntil)
			floodUntil = time.Now().Add(time.Duration(waitSecs+2) * time.Second)
			editMu.Unlock()

			if alreadyKnown {
				return true // आधीच notify झालंय, परत spam नको
			}
			utils.Logger.Sugar().Warnw("predownload: FLOOD_WAIT आला — status updates तात्पुरते थांबवतोय",
				"waitSecs", waitSecs, "context", context_)
			nCtx, nCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = botClient.API().MessagesSendMessage(nCtx, &tg.MessagesSendMessageRequest{
				Peer: adminPeer,
				Message: fmt.Sprintf(
					"⏳ Telegram FLOOD_WAIT (%s) — सुमारे %s साठी status updates थांबतील, नंतर आपोआप सुरू होतील. (Download/upload मात्र चालूच राहील.)",
					context_, time.Duration(waitSecs)*time.Second,
				),
				RandomID: rand.Int63(),
			})
			nCancel()
			return true
		}

		inFloodWindow := func() bool {
			editMu.Lock()
			defer editMu.Unlock()
			return time.Now().Before(floodUntil)
		}

		rotateStatusMsg := func(text string) {
			if inFloodWindow() {
				return
			}
			editMu.Lock()
			oldID := statusMsgID
			editMu.Unlock()

			sCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
			newMsg, sErr := botClient.API().MessagesSendMessage(sCtx, &tg.MessagesSendMessageRequest{
				Peer: adminPeer, Message: text, RandomID: rand.Int63(),
			})
			sCancel()
			if sErr != nil {
				if waitSecs, isFlood := extractFloodWaitSeconds(sErr); isFlood {
					markFlood(waitSecs, "नवीन status message")
				}
				return
			}
			if newID := extractMessageID(newMsg); newID != 0 {
				editMu.Lock()
				statusMsgID = newID
				msgCreatedAt = time.Now()
				editMu.Unlock()
			}
			if oldID != 0 {
				delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, _ = botClient.API().MessagesDeleteMessages(delCtx, &tg.MessagesDeleteMessagesRequest{
					Revoke: true, ID: []int{oldID},
				})
				delCancel()
			}
		}

		editProgress := func(text string) {
			if inFloodWindow() {
				return // [NEW] Flood window संपेपर्यंत API ला अजिबात touch नाही
			}

			editMu.Lock()
			// [FIX] 1s → 6s: FLOOD_WAIT टाळण्यासाठी edits मधलं अंतर वाढवलं —
			// आधी itka वारंवार edits केल्यामुळे Telegram कडून
			// rpc error code 420: FLOOD_WAIT येत होता.
			if time.Since(lastEdit) < 6*time.Second {
				editMu.Unlock()
				return
			}
			lastEdit = time.Now()

			// [NEW] Status message 10 मिनिटांपेक्षा जुना झाला असेल तर तो
			// delete करून नवीन पाठवतो (rotate) — जेणेकरून एकाच message वर
			// edits ची संख्या मर्यादित राहील आणि पुढेही FLOOD_WAIT येणार नाही.
			if time.Since(msgCreatedAt) >= statusMsgMaxAge {
				editMu.Unlock()
				rotateStatusMsg(text)
				return
			}
			currentID := statusMsgID
			editMu.Unlock()

			if currentID != 0 {
				eCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := botClient.API().MessagesEditMessage(eCtx, &tg.MessagesEditMessageRequest{
					Peer: adminPeer, ID: currentID, Message: text,
				})
				cancel()
				if err == nil {
					return
				}

				// [FIX] नेमका FLOOD_WAIT असेल तर आता synchronously wait/retry
				// न करता floodUntil सेट करतो — तेवढा वेळ संपेपर्यंत या
				// function मधलं वरचं `inFloodWindow()` check सगळे पुढचे
				// attempts आपोआप skip करेल.
				if waitSecs, isFlood := extractFloodWaitSeconds(err); isFlood {
					markFlood(waitSecs, "progress edit")
					return
				}

				// [NEW] Edit fail झाला (FLOOD_WAIT नसलेल्या कारणाने) — admin
				// ने मध्येच status message delete केला असेल (MESSAGE_ID_INVALID
				// वगैरे). Process थांबवायची गरज नाही — फक्त एक नवीन status
				// message पाठवून पुढच्या updates साठी तोच वापरायचा, जेणेकरून
				// live progress परत दिसू लागेल.
				utils.Logger.Sugar().Warnw("predownload: progress edit failed — status message delete झाला असेल, नवीन पाठवतोय", "err", err)
			}

			rotateStatusMsg(text)
		}

		// [NEW] नवीन (uncached) lectures साठी concurrency — किती watermark
		// workers प्रत्यक्ष online आहेत (ActiveWorkers, DB-backed poll-tracking)
		// त्यावर आधारित. प्रत्येक worker (watermark_worker.py, MAX_CONCURRENT_JOBS)
		// आता एकाच वेळी jobsPerWorker jobs परत घेऊ शकतो, त्यामुळे एकूण
		// system capacity = online workers × jobsPerWorker — पण जास्तीत
		// जास्त कॅप लावलाय जेणेकरून एकाच वेळी शेकडो jobs एकदम submit होऊन
		// इतर live-user requests (न्यू watermark jobs) साठी queue busy
		// होणार नाही.
		jobsPerWorker := wmqueue.Default.DynamicMaxJobsPerWorker()
		freshConcurrency := len(wmqueue.ActiveWorkers()) * jobsPerWorker
		if freshConcurrency < 1 {
			freshConcurrency = 1 // कुठलाही worker online नाही तरी एकेक करून प्रयत्न करत राहतो
		}
		const maxFreshConcurrency = 50 // [FIX] Increased cap from 6 to 50 to allow multiple Colab workers to run at full capacity
		if freshConcurrency > maxFreshConcurrency {
			freshConcurrency = maxFreshConcurrency
		}

		// [FIX] आधीच cache (DeliveredCache) असलेल्या lectures ला watermark
		// worker लागतच नाही — फक्त एक Mongo lookup + log-channel forward आहे.
		// त्यामुळे या batch ला worker-count शी बांधून concurrency मर्यादित
		// करायचं काहीच कारण नाही — जास्त concurrency (cachedConcurrency)
		// वापरून हे लगेच संपतं. फक्त प्रत्यक्ष नवीन processing लागणाऱ्या
		// lectures साठीच worker-based concurrency लागू होते.
		const cachedConcurrency = 20
		var cachedIDs, freshIDs []int
		for _, msgID := range msgIDs {
			if c, cErr := db.GetDeliveredCache(targetUser.ID, courseID, msgID); cErr == nil && c != nil {
				cachedIDs = append(cachedIDs, msgID)
			} else {
				freshIDs = append(freshIDs, msgID)
			}
		}

		var mu sync.Mutex
		done, skipped, failed := 0, 0, 0
		processed := 0
		processing := 0 // [NEW] सध्या in-flight (active) jobs — फक्त progress-bar साठी

		// [NEW] सध्या in-flight असलेल्या प्रत्येक lecture चा msgID→colab jobID —
		// जेणेकरून प्रत्येक lecture चा live stage/percent (wmqueue.ProgressMessage)
		// वाचून progress-bar मध्ये per-job detail दाखवता येईल. insertion-order
		// राखण्यासाठी slice + map दोन्ही (map मधलं iteration order random असतं).
		var processingOrder []int
		jobIDByMsg := make(map[int]string)

		unregisterHook := AddJobSubmittedHook(func(messageID int, jobID string) {
			mu.Lock()
			jobIDByMsg[messageID] = jobID
			mu.Unlock()
		})
		defer unregisterHook()

		// [NEW] 5-सेकंद visual progress-bar ticker — existing per-completion
		// editProgress ला touch न करता, वेगळ्या ticker मधून पूर्ण-batch चा
		// एकत्रित overview (+ प्रत्येक चालू lecture चा stage/percent) पाठवतो.


		progressStopCh := make(chan struct{})
		StartProgressBarTicker(
			progressStopCh,
			fmt.Sprintf("⏳ Pre-download\n👤 %s\n📚 %s", targetUser.FullName, course.Title),
			func() (int, int, int, int, int) {
				mu.Lock()
				defer mu.Unlock()
				return done, skipped, failed, processing, total
			},
			func() []string {
				mu.Lock()
				order := append([]int(nil), processingOrder...)
				jobs := make(map[int]string, len(jobIDByMsg))
				for k, v := range jobIDByMsg {
					jobs[k] = v
				}
				mu.Unlock()

				lines := make([]string, 0, len(order))
				for _, msgID := range order {
					stage := "⏳ सुरू होतंय..."
					if jobID, ok := jobs[msgID]; ok {
						if msg, ok2 := wmqueue.ProgressMessage(jobID); ok2 {
							stage = msg
						}
					}
					lines = append(lines, fmt.Sprintf("• Lecture %d — %s", msgID, stage))
				}
				return lines
			},
			editProgress,
		)
		defer close(progressStopCh)

		runBatch := func(ids []int, isFresh bool) {
			if len(ids) == 0 || bgCtx.Err() != nil {
				return
			}
			var wg sync.WaitGroup
			for _, msgID := range ids {
				if bgCtx.Err() != nil {
					break
				}

				// Dynamic Concurrency Control:
				// If fresh (watermark required), dynamically evaluate active worker capacity
				for {
					if bgCtx.Err() != nil {
						break
					}
					mu.Lock()
					active := processing
					mu.Unlock()

					targetConc := cachedConcurrency
					if isFresh {
						targetConc = len(wmqueue.ActiveWorkers()) * wmqueue.Default.DynamicMaxJobsPerWorker()
						if targetConc < 1 {
							targetConc = 1
						}
						if targetConc > maxFreshConcurrency {
							targetConc = maxFreshConcurrency
						}
					}

					if active < targetConc {
						break
					}
					time.Sleep(250 * time.Millisecond)
				}

				if bgCtx.Err() != nil {
					break
				}

				wg.Add(1)
				mu.Lock()
				processing++
				processingOrder = append(processingOrder, msgID)
				mu.Unlock()

				go func(msgID int) {
					defer wg.Done()

					if bgCtx.Err() != nil {
						return
					}

					delivered, dErr := runWithFloodRetryGeneric(bgCtx, 5, func() (bool, error) {
						return deliverLectureCore(ctx, botClient, targetUser, course, courseID, msgID, targetUser.TelegramID, 0, true, func(string) {})
					})

					mu.Lock()
					processed++
					processing--
					for i, id := range processingOrder {
						if id == msgID {
							processingOrder = append(processingOrder[:i], processingOrder[i+1:]...)
							break
						}
					}
					delete(jobIDByMsg, msgID)
					if dErr != nil {
						failed++
						utils.Logger.Sugar().Warnw("predownload: lecture delivery failed", "msgID", msgID, "err", dErr)
					} else if delivered {
						done++
					} else {
						skipped++
					}

					liveLimit := cachedConcurrency
					if isFresh {
						liveLimit = len(wmqueue.ActiveWorkers()) * wmqueue.Default.DynamicMaxJobsPerWorker()
						if liveLimit < 1 {
							liveLimit = 1
						}
					}
					editProgress(fmt.Sprintf(
						"⏳ Pre-download %d/%d (parallel: %d active / max %d)\n👤 %s\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n\n🛑 Stop: /stopuserbotdl",
						processed, total, processing, liveLimit, targetUser.FullName, done, skipped, failed,
					))
					mu.Unlock()
				}(msgID)
			}
			wg.Wait()
		}

		// Phase 1: आधीच cached असलेले — जलद, उच्च concurrency.
		runBatch(cachedIDs, false)
		// Phase 2: खरंच नवीन (watermark worker लागणारे) — dynamic worker scaling concurrency.
		runBatch(freshIDs, true)

		stoppedNote := ""
		if bgCtx.Err() != nil {
			stoppedNote = "\n🛑 *Process stopped*"
		}
		summary := fmt.Sprintf(
			"📊 *Pre-download Summary*\n👤 %s\n📚 %s\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d%s",
			targetUser.FullName, course.Title, done, skipped, failed, stoppedNote,
		)
		fCtx, fCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer fCancel()
		_, _ = botClient.API().MessagesSendMessage(fCtx, &tg.MessagesSendMessageRequest{
			Peer: adminPeer, Message: summary, RandomID: rand.Int63(),
		})
	}()

	return dispatcher.EndGroups
}

// runWithFloodRetryGeneric — [NEW] runWithFloodRetry सारखंच, पण bool+error
// परत करणाऱ्या function साठी (deliverLectureCore चा return-shape वेगळा
// असल्यामुळे वेगळी generic आवृत्ती — डुप्लिकेशन टाळायला दोन्हीत समान core वापरता
// येईल इतकं छोटं आहे, वेगळं ठेवणंच सोपं).
func runWithFloodRetryGeneric(bgCtx context.Context, maxFloodRetries int, fn func() (bool, error)) (bool, error) {
	var ok bool
	var err error
	for attempt := 0; attempt <= maxFloodRetries; attempt++ {
		if bgCtx.Err() != nil {
			return false, bgCtx.Err()
		}
		ok, err = fn()
		if err == nil {
			return ok, nil
		}
		waitSecs, isFlood := extractFloodWaitSeconds(err)
		if !isFlood || attempt == maxFloodRetries {
			return ok, err
		}
		select {
		case <-time.After(time.Duration(waitSecs+2) * time.Second):
		case <-bgCtx.Done():
			return false, bgCtx.Err()
		}
	}
	return ok, err
}


// [NEW] Emergency data-migration साठी: source channel मधून destination channel
// कडे (दोन्ही वेगळे, admin-दिलेले) सगळे messages — media + caption सकट —
// थेट download+re-upload करतो. "Restrict saving content" (forward off) असला
// तरी चालतं, कारण हे forward/copy_message वापरतच नाही — userbot प्रत्यक्ष file
// download करतो (bot तिथला admin/member असल्यामुळे legitimate access आहे),
// आणि bot destination ला ती नव्याने upload करतो. हेच engine (`prepareMessage`
// / `sendPreparedMessage`) /userbotdl वापरतं — इथे फक्त destination म्हणून
// admin-दिलेला arbitrary channel पास केलाय (globally-configured SendChannel
// ऐवजी). /stopuserbotdl नेच हे पण थांबवता येतं (तोच shared cancel-state).
func cmdCopyChannel(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	a := args(u)

	if len(a) < 3 {
		reply(ctx, u,
			"📤 *Usage:*\n\n"+
				"`/copychannel <sourceChatID> <destChatID> <msgID>`\n"+
				"→ Single message\n\n"+
				"`/copychannel <sourceChatID> <destChatID> <start> <end>`\n"+
				"→ Range of messages\n\n"+
				"*Examples:*\n"+
				"`/copychannel 2479194095 2588123456 5654`\n"+
				"`/copychannel 2479194095 2588123456 1 500`\n\n"+
				"⚡ Watermark set नसेल तर जलद (reference-copy, download न करता) पद्धत आपोआप वापरली जाईल.\n"+
				"⚠️ Watermark set असेल तर download+reupload होईल (जास्त वेळ लागेल) — शुद्ध/जलद copy हवा असेल तर आधी /clearwatermark कर.\n"+
				"⚠️ Bot दोन्ही channels मध्ये admin असायलाच हवा.\n"+
				"Stop करायला: /stopuserbotdl")
		return dispatcher.EndGroups
	}

	// sourceChatID parse — /userbotdl सारखंच (t.me/c/ चा positive ID)
	parseChanID := func(raw string) (int64, int64, error) {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v == 0 {
			return 0, 0, fmt.Errorf("invalid channel ID: %s", raw)
		}
		if v > 0 {
			return v, -1000000000000 - v, nil
		}
		return v, v, nil
	}

	srcRaw, sourceChatID, err := parseChanID(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid sourceChatID.\nt.me/c/ वरील positive ID वापरा.\nExample: `2479194095`")
		return dispatcher.EndGroups
	}
	destRaw, destChatID, err := parseChanID(a[1])
	if err != nil {
		reply(ctx, u, "❌ Invalid destChatID.\nt.me/c/ वरील positive ID वापरा.\nExample: `2588123456`")
		return dispatcher.EndGroups
	}

	var startMsg, endMsg int
	switch len(a) {
	case 3:
		msgID, err := strconv.Atoi(a[2])
		if err != nil || msgID < 1 {
			reply(ctx, u, "❌ Invalid messageID. Positive number असायला हवे.")
			return dispatcher.EndGroups
		}
		startMsg, endMsg = msgID, msgID
	case 4:
		s, err1 := strconv.Atoi(a[2])
		e, err2 := strconv.Atoi(a[3])
		if err1 != nil || err2 != nil || s < 1 || e < 1 {
			reply(ctx, u, "❌ Invalid range. दोन्ही positive numbers असायला हवेत.")
			return dispatcher.EndGroups
		}
		if s > e {
			reply(ctx, u, fmt.Sprintf("❌ Start (%d) > End (%d) — start ≤ end असायला हवे.", s, e))
			return dispatcher.EndGroups
		}
		startMsg, endMsg = s, e
	default:
		reply(ctx, u, "❌ जास्त arguments. Format बघा: `/copychannel`")
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}
	// [FIX] userbot इथे compulsory नाही — bot आधीच दोन्ही channels मध्ये admin
	// असल्यामुळे fast-path (watermark नसताना) ला userbot ची अजिबात गरज नाही.
	// फक्त watermark ON असेल, किंवा fast-path fail होऊन fallback लागला, तरच
	// खाली userbot lazily वापरला जातो — तेव्हा तो nil असेल तर तिथेच स्पष्ट
	// error येईल (संपूर्ण command सुरुवातीलाच अडणार नाही).
	var userbotClient *gotgproto.Client
	if GetUserbotClient != nil {
		userbotClient = GetUserbotClient()
	}

	// Destination channel मध्ये bot खरंच admin/access आहे का ते आधीच तपासतो —
	// नाहीतर हजारो messages नंतर मध्येच fail होत राहील.
	destPeerRaw, dErr := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, destChatID)
	if dErr != nil || destPeerRaw == nil {
		reply(ctx, u, fmt.Sprintf("❌ Destination channel (`%d`) मध्ये bot ला access नाही — आधी bot ला तिथे admin बनव.\nError: %s", destRaw, dErr))
		return dispatcher.EndGroups
	}
	destPeer := &tg.InputPeerChannel{ChannelID: destPeerRaw.ChannelID, AccessHash: destPeerRaw.AccessHash}


	adminChatID := u.EffectiveChat().GetID()
	botPeer := botClient.PeerStorage.GetInputPeerById(adminChatID)

	totalMsgs := endMsg - startMsg + 1
	rangeStr := fmt.Sprintf("Msg `%d`", startMsg)
	if startMsg != endMsg {
		rangeStr = fmt.Sprintf("Msgs `%d` → `%d` (%d total)", startMsg, endMsg, totalMsgs)
	}

	// [FIX] Watermark ON असेल तर fast-path वापरताच येणार नाही — पूर्ण
	// download+reupload साठी userbot compulsory आहे. ते आधीच (goroutine
	// सुरू करण्याआधीच) तपासतो, नाहीतर हजारो messages नंतर मध्येच अडेल.
	if _, wmOk := getWatermarkText(); wmOk && userbotClient == nil {
		reply(ctx, u, "❌ Watermark सध्या ON आहे, त्यामुळे पूर्ण download+reupload लागेल — त्यासाठी userbot चालू असायला हवा, आणि तो चालू नाहीये.\n\nपर्याय:\n• `/clearwatermark` कर (मग जलद reference-copy वापरली जाईल, userbot शिवाय चालेल), किंवा\n• Admin ला सांग userbot session set करायला.")
		return dispatcher.EndGroups
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
		"⏳ Starting copy...\n📡 Source: `%d` → Dest: `%d`\n📋 %s\n\n🛑 Stop: /stopuserbotdl",
		srcRaw, destRaw, rangeStr,
	)), nil)
	sentMsgID := 0
	if sentMsg != nil {
		sentMsgID = sentMsg.ID
	}

	go func() {
		bgCtx, bgCancel := context.WithCancel(context.Background())
		pID := addActiveProcess(senderID, bgCancel)
		defer func() {
			bgCancel()
			removeActiveProcess(pID)
		}()

		var editMu sync.Mutex
		var lastEdit time.Time
		var editCount int
		var msgCreatedAt = time.Now()

		editProgress := func(text string) {
			editMu.Lock()
			if sentMsgID == 0 {
				editMu.Unlock()
				return
			}
			
		isFinalMsg := strings.HasPrefix(text, "❌") || strings.HasPrefix(text, "✅")
			if isFinalMsg {
			editMu.Lock()
			currentMsgID := sentMsgID
			editMu.Unlock()
			if currentMsgID != 0 {
				botToken := os.Getenv("BOT_TOKEN")
				wmqueue.DeleteTgMessage(botToken, u.EffectiveChat().GetID(), int64(currentMsgID))
				newID := wmqueue.SendTgMessage(botToken, u.EffectiveChat().GetID(), text)
				if newID != 0 {
					editMu.Lock()
					sentMsgID = int(newID)
					editMu.Unlock()
				}
			}
			return
		}
// 5s frequency as requested by user
			if time.Since(lastEdit) < 5*time.Second {
				editMu.Unlock()
				return
			}
			botToken := os.Getenv("BOT_TOKEN")
			needsNewMsg := false
			if editCount >= 10 || time.Since(msgCreatedAt) >= 2*time.Minute {
				needsNewMsg = true
			} else if _, waiting := wmqueue.InEditFloodWait(botToken); waiting {
				needsNewMsg = true
				wmqueue.ClearEditFloodWait(botToken)
			}
			
			lastEdit = time.Now()
			currentMsgID := sentMsgID
			editMu.Unlock()

			go func(msgID int) {
				if needsNewMsg {
					newID := wmqueue.SendTgMessage(botToken, adminChatID, text)
					if newID != 0 {
						editMu.Lock()
						sentMsgID = int(newID)
						editCount = 0
						msgCreatedAt = time.Now()
						editMu.Unlock()
						wmqueue.DeleteTgMessage(botToken, adminChatID, int64(msgID))
					}
					return
				}

				// Otherwise edit
				success := wmqueue.EditTgMessage(botToken, adminChatID, msgID, text)
				if !success {
					// Edit failed (possibly FloodWait), delete and send new
					newID := wmqueue.SendTgMessage(botToken, adminChatID, text)
					if newID != 0 {
						editMu.Lock()
						sentMsgID = int(newID)
						editCount = 0
						msgCreatedAt = time.Now()
						editMu.Unlock()
						wmqueue.DeleteTgMessage(botToken, adminChatID, int64(msgID))
					}
				} else {
					editMu.Lock()
					editCount++
					editMu.Unlock()
				}
			}(currentMsgID)
		}
		deleteProgress := func() {
			if sentMsgID == 0 {
				return
			}
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = botClient.API().MessagesDeleteMessages(delCtx, &tg.MessagesDeleteMessagesRequest{
				Revoke: true, ID: []int{sentMsgID},
			})
		}

		successCount, skipCount, errorCount := 0, 0, 0
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
				"📊 *Copy Summary*\n📡 `%d` → `%d`\n📋 Range: `%d`→`%d`\n\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d%s",
				srcRaw, destRaw, startMsg, endMsg, successCount, skipCount, errorCount, statusLine,
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
						s += fmt.Sprintf("\n• Msgs %s — `%s`", strings.Join(idStrs, ", "), errText)
					}
				}
			}
			return s
		}

		var summaryEditMu sync.Mutex
		var summaryEditCount int
		var summaryCreatedAt = time.Now()

		updateSummary := func() {
			botToken := os.Getenv("BOT_TOKEN")
			text := buildSummaryText()
			
			summaryEditMu.Lock()
			if summaryMsgID == 0 {
				summaryEditMu.Unlock()
				newID := wmqueue.SendTgMessage(botToken, adminChatID, text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
				}
				return
			}

			needsNewMsg := false
			if summaryEditCount >= 10 || time.Since(summaryCreatedAt) >= 2*time.Minute {
				needsNewMsg = true
			} else if _, waiting := wmqueue.InEditFloodWait(botToken); waiting {
				needsNewMsg = true
				wmqueue.ClearEditFloodWait(botToken)
			}
			currentSummaryID := summaryMsgID
			summaryEditMu.Unlock()

			if needsNewMsg {
				newID := wmqueue.SendTgMessage(botToken, adminChatID, text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
					wmqueue.DeleteTgMessage(botToken, adminChatID, int64(currentSummaryID))
				}
				return
			}

			success := wmqueue.EditTgMessage(botToken, adminChatID, currentSummaryID, text)
			if !success {
				newID := wmqueue.SendTgMessage(botToken, adminChatID, text)
				if newID != 0 {
					summaryEditMu.Lock()
					summaryMsgID = int(newID)
					summaryEditCount = 0
					summaryCreatedAt = time.Now()
					summaryEditMu.Unlock()
					wmqueue.DeleteTgMessage(botToken, adminChatID, int64(currentSummaryID))
				}
			} else {
				summaryEditMu.Lock()
				summaryEditCount++
				summaryEditMu.Unlock()
			}
		}

		// [NEW] Watermark set नसेल तर fastCopyMessage (reference-based, कुठलाही
		// download/reupload नाही) वापरतो — watermark server offline असतानाही
		// live "video request" flow जसं लगेच video देतं, तसंच इथेही जलद copy
		// होतं. Watermark SET असेल तरच जड (download+FFmpeg+reupload) engine
		// वापरायची गरज आहे.
		_, wmOk := getWatermarkText()
		useFastPath := !wmOk

		runPhaseA := func(msgID int) (*preparedMessage, error) {
			const maxRetries = 2
			var pm *preparedMessage
			var err error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if bgCtx.Err() != nil {
					return nil, err
				}
				if attempt > 1 {
					editProgress(fmt.Sprintf("🔄 Retry %d/%d\n📌 Msg: `%d`", attempt, maxRetries, msgID))
					time.Sleep(5 * time.Second)
				}
				// [FIX] sourcePeer (nil) — copy फक्त destination ला जावी, source
				// channel मध्ये परत डुप्लिकेट नको. sendChannelPeer = destPeer.
				pm, err = prepareMessage(
					bgCtx, userbotClient, botClient, sourceChatID, srcRaw, msgID, 0,
					botPeer, nil, destPeer, editProgress, adminChatID, &sentMsgID,
				)
				if err == nil {
					return pm, nil
				}
				if bgCtx.Err() != nil {
					return nil, err
				}
				if strings.Contains(strings.ToLower(err.Error()), "deleted") {
					return nil, err
				}
			}
			return nil, err
		}

		type phaseAOutcome struct {
			pm  *preparedMessage
			err error
		}
		var pendingPhaseA *phaseAOutcome
		var pendingMsgID int

		msgIDList := make([]int, 0, totalMsgs)
		for id := startMsg; id <= endMsg; id++ {
			msgIDList = append(msgIDList, id)
		}

		for msgIdx, msgID := range msgIDList {
			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			currentIdx := msgIdx + 1

			// ── [NEW] Fast path — watermark नसेल तेव्हा, कुठलाही download न
			// करता थेट reference-copy (एका API call मध्ये पूर्ण होतं, Phase A/B
			// pipeline ची गरज नाही).
			if useFastPath {
				editProgress(fmt.Sprintf(
					"⚡ Copying %d/%d (fast, no watermark)\n📡 `%d` → `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n\n🛑 Stop: /stopuserbotdl",
					currentIdx, totalMsgs, srcRaw, destRaw, msgID, successCount, skipCount, errorCount,
				))

				fcErr := runWithFloodRetry(bgCtx, editProgress, msgID, 8, func() error {
					return fastCopyMessage(bgCtx, botClient, sourceChatID, msgID, destPeer)
				})

				if fcErr != nil {
					if bgCtx.Err() != nil {
						stopped = true
						deleteProgress()
						updateSummary()
						return
					}
					// [FALLBACK] Fast-path fail झाला (FloodWait नाही, दुसरंच काहीतरी —
					// उदा. FILE_REFERENCE_EXPIRED) → जड (download+reupload) pipeline
					// try करतो, जेणेकरून हा message skip न होता तरी copy होईल.
					// हा मार्ग userbot शिवाय शक्य नाही — इथेच nil check.
					var pmErr error
					var pm *preparedMessage
					if userbotClient == nil {
						pmErr = fmt.Errorf("userbot चालू नाही — fallback शक्य नाही (fast-path error: %v)", fcErr)
					} else {
						editProgress(fmt.Sprintf(
							"⚠️ Fast copy fail (%v) → full download-fallback...\n📌 Msg: `%d`",
							fcErr, msgID,
						))
						pm, pmErr = runPhaseA(msgID)
						if pmErr == nil {
							pmErr = runWithFloodRetry(bgCtx, editProgress, msgID, 8, func() error {
								return sendPreparedMessage(
									bgCtx, userbotClient, botClient, sourceChatID, srcRaw, 0,
									botPeer, nil, destPeer, editProgress, pm,
								)
							})
						}
					}
					if pmErr != nil {
						errorCount++
						errKey := fmt.Sprintf("fast+fallback both failed: %v", pmErr)
						if _, exists := errorLines[errKey]; !exists {
							errorOrder = append(errorOrder, errKey)
						}
						errorLines[errKey] = append(errorLines[errKey], msgID)
						updateSummary()
						if totalMsgs > 1 {
							time.Sleep(1 * time.Second)
						}
						continue
					}
				}

				successCount++
				updateSummary()
				if totalMsgs > 1 && msgIdx < len(msgIDList)-1 {
					time.Sleep(300 * time.Millisecond) // fast path — रेफरन्स-कॉपीच आहे, कमी delay पुरेसा
				}
				continue
			}

			editProgress(fmt.Sprintf(
				"⏳ Copying %d/%d\n📡 `%d` → `%d` | Msg: `%d`\n✅ Done: %d | ⏭ Skip: %d | ❌ Err: %d\n\n🛑 Stop: /stopuserbotdl",
				currentIdx, totalMsgs, srcRaw, destRaw, msgID, successCount, skipCount, errorCount,
			))

			var phA phaseAOutcome
			if pendingPhaseA != nil && pendingMsgID == msgID {
				phA = *pendingPhaseA
				pendingPhaseA = nil
			} else {
				pm, err := runPhaseA(msgID)
				phA = phaseAOutcome{pm: pm, err: err}
			}

			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			if phA.err != nil {
				errorCount++
				errKey := phA.err.Error()
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

			var phaseBErr error
			var phaseBWg sync.WaitGroup
			phaseBWg.Add(1)
			go func(pm *preparedMessage) {
				defer phaseBWg.Done()
				phaseBErr = runWithFloodRetry(bgCtx, editProgress, msgID, 8, func() error {
					return sendPreparedMessage(
						bgCtx, userbotClient, botClient, sourceChatID, srcRaw, 0,
						botPeer, nil, destPeer, editProgress, pm,
					)
				})
			}(phA.pm)

			if msgIdx+1 < len(msgIDList) && bgCtx.Err() == nil {
				nextMsgID := msgIDList[msgIdx+1]
				pm, err := runPhaseA(nextMsgID)
				pendingPhaseA = &phaseAOutcome{pm: pm, err: err}
				pendingMsgID = nextMsgID
			}

			phaseBWg.Wait()

			if bgCtx.Err() != nil {
				stopped = true
				deleteProgress()
				updateSummary()
				return
			}

			if phaseBErr != nil {
				errorCount++
				errKey := phaseBErr.Error()
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

			if totalMsgs > 1 && msgIdx < len(msgIDList)-1 {
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

// ── preparedMessage ────────────────────────────────────────────────────────────
// Phase A (prepareMessage) चं output — Phase B (sendPreparedMessage) ला लागणारं
// सगळं state इथे साठवलं जातं. Pipeline मध्ये Phase A(N+1) आणि Phase B(N)
// parallel चालतात, म्हणून हे struct goroutines मध्ये pass केलं जातं.

type preparedMessage struct {
	textOnly      bool // true असेल तर Phase B ने काही करायची गरज नाही (Phase A ने सगळं केलं)
	colabSent     bool // true असेल तर Colab ने already Telegram la pathavle — Phase B skip
	tgMsg         *tg.Message
	finalCaption  string
	file          *types.File
	uploadedFile  tg.InputFileClass
	msgID         int
	tempVideoPath string // FFmpeg thumb fallback sathi — sendPreparedMessage use karto, nantar delete
}

// ── prepareMessage ───────────────────────────────────────────────────────────
// Phase A: fetch message, caption prepare, text-only handle, media download +
// (watermark असेल तर Colab submit+wait) → uploadedFile/uploaded-to-colab.
// Telegram कडे final media send करत नाही (ते Phase B करतो) — सिवाय text-only
// आणि colab-sent cases, जे इथेच पूर्ण होतात.

func prepareMessage(
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
	adminChatID int64,
	sentMsgID *int,
) (*preparedMessage, error) {

	// ── Step 1: Fetch message ─────────────────────────────────────────────
	tgMsg, err := utils.GetTGMessageInChannel(bgCtx, userbotClient, msgID, chatID)
	if err != nil {
		return nil, fmt.Errorf("message fetch failed: %w", err)
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
				return nil, fmt.Errorf("text send failed: %w", err)
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
		return &preparedMessage{textOnly: true}, nil
	}

	// ── Step 4: Media → download + reupload ──────────────────────────────
	file, err := utils.FileFromMessageInChannel(bgCtx, userbotClient, msgID, chatID)
	if err != nil {
		return nil, fmt.Errorf("file metadata error: %w", err)
	}

	editProgress(fmt.Sprintf(
		"📥 Downloading...STEP.1/3\n📄 `%s`\n💾 %s\n📌 Chat: `%d` | Msg: `%d`\n░░░░░░░░░░ 0%%",
		file.FileName, formatSize(file.FileSize), chatIDRaw, msgID,
	))

	stallCtx, stallCancel := context.WithCancel(bgCtx)
	defer stallCancel()
	stallTimer := time.AfterFunc(30*time.Minute, stallCancel)

	var dlStartTime time.Time
	var dlLastTime time.Time
	var dlLastWritten int64
	var dlSpeedStr string = "0.00 MB/s"

	onDownloadProgress := func(written, total int64) {
		stallTimer.Reset(30 * time.Minute)
		if total <= 0 {
			return
		}

		now := time.Now()
		if dlStartTime.IsZero() {
			dlStartTime = now
			dlLastTime = now
			dlLastWritten = written
		} else {
			elapsed := now.Sub(dlLastTime).Seconds()
			if elapsed >= 1.0 { // update speed every 1s
				bytesPerSec := float64(written-dlLastWritten) / elapsed
				dlSpeedStr = fmt.Sprintf("%.2f MB/s", bytesPerSec/(1024*1024))
				dlLastTime = now
				dlLastWritten = written
			}
		}

		pct := written * 100 / total
		bar := progressBar(int(pct))
		editProgress(fmt.Sprintf(
			"📥 (Phase 1) Downloading...STEP.1/3\n📄 `%s`\n⚡ Speed: %s\n💾 %s / %s\n%s %d%%\n📌 Msg: `%d`",
			file.FileName,
			dlSpeedStr,
			formatSize(written), formatSize(total),
			bar, pct, msgID,
		))
	}

	var upStartTime time.Time
	var upLastTime time.Time
	var upLastWritten int64
	var upSpeedStr string = "0.00 MB/s"

	onUploadProgress := func(written, total int64) {
		if total <= 0 {
			return
		}

		now := time.Now()
		if upStartTime.IsZero() {
			upStartTime = now
			upLastTime = now
			upLastWritten = written
		} else {
			elapsed := now.Sub(upLastTime).Seconds()
			if elapsed >= 1.0 { // update speed every 1s
				bytesPerSec := float64(written-upLastWritten) / elapsed
				upSpeedStr = fmt.Sprintf("%.2f MB/s", bytesPerSec/(1024*1024))
				upLastTime = now
				upLastWritten = written
			}
		}
		pct := written * 100 / total
		bar := progressBar(int(pct))
		editProgress(fmt.Sprintf(
			"📤 Uploading...STEP.1/3\n📄 `%s`\n⚡ Speed: %s\n💾 %s / %s\n%s %d%%\n📌 Msg: `%d`",
			file.FileName, upSpeedStr,
			formatSize(written), formatSize(total),
			bar, pct, msgID,
		))
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer upCancel()

	// ── Step 4b: Watermark apply (FFmpeg) ────────────────────────────────
	// Watermark set असेल तर download → FFmpeg overlay → reupload
	wmText, wmOk := getWatermarkTextForUser(adminChatID)
	if !wmOk || wmText == "" {
		wmText, wmOk = getWatermarkText()
	}

	var uploadedFile tg.InputFileClass
	var colabSent bool
	var pm_tempPath string // non-watermark path sathi FFmpeg thumb fallback

	koyebURLCheck := os.Getenv("KOYEB_URL")
	// [FALLBACK] Colab/watermark worker configured असला तरी सध्या कुठलाही
	// worker online नसेल (wmqueue.IsColabOnline — RecordPoll based tracker)
	// तर Colab la job submit करण्यात अर्थ नाही — तो job pending राहून
	// upCtx (30 min) timeout नंतरच fail होईल. त्याऐवजी लगेच main server
	// वरच download+reupload कर (watermark शिवाय — main server कडे FFmpeg
	// drawtext capability नाहीये, फक्त Colab worker कडे आहे).
	colabConfigured := koyebURLCheck != "" && isVideoFile(file)
	colabOnlineNow := colabConfigured && wmqueue.IsColabOnline()

	if colabOnlineNow {
		// Sarv videos Colab kade jातात — watermark असो वा नसो.
		// Main server फक्त pipe सर्व्हर म्हणून काम करतो, free राहतो.
		statusLabel := "Downloading for watermark..."
		if !wmOk {
			statusLabel = "Downloading (Colab processing)..."
		}
		editProgress(fmt.Sprintf(
			"📥 %s\n📄 `%s`\n💾 %s\n📌 Msg: `%d`",
			statusLabel, file.FileName, formatSize(file.FileSize), msgID,
		))

		// Bot token + Colab job saathi send target
		botToken := os.Getenv("BOT_TOKEN")
		var colabSendChatID int64
		var colabSendAccessHash int64

		if scID, ok := getSendChannelIDForUser(adminChatID); ok && scID != 0 {
			colabSendChatID = scID
			if sendChannelPeer != nil {
				colabSendAccessHash = sendChannelPeer.AccessHash
			}
		} else if sendChannelPeer != nil {
			idStr := fmt.Sprintf("-100%d", sendChannelPeer.ChannelID)
			colabSendChatID, _ = strconv.ParseInt(idStr, 10, 64)
			colabSendAccessHash = sendChannelPeer.AccessHash
		} else {
			colabSendChatID = adminChatID
		}

		// LOG_CHANNEL backup — jujha aadhi pratyek file la hoti
		var colabLogChatID int64
		var colabLogAccessHash int64
		if logCh, logErr := utils.GetLogChannelPeer(stallCtx, botClient.API(), botClient.PeerStorage); logErr == nil && logCh != nil {
			logIDStr := fmt.Sprintf("-100%d", logCh.ChannelID)
			colabLogChatID, _ = strconv.ParseInt(logIDStr, 10, 64)
			colabLogAccessHash = logCh.AccessHash
			utils.Logger.Info("Colab job: log channel resolved",
				zap.Int64("logChatID", colabLogChatID))
		} else {
			utils.Logger.Warn("Colab job: log channel peer FAILED — backup skip होईल",
				zap.Error(logErr))
		}

		finalCaptionForColab := finalCaption
		if finalCaptionForColab == "" {
			finalCaptionForColab = file.FileName
		}

		uploadedFile, colabSent, err = downloadWatermarkAndReupload(
			stallCtx,
			upCtx,
			userbotClient,
			botClient,
			file,
			wmText,
			onDownloadProgress,
			func(written, total int64) {
				stallTimer.Reset(30 * time.Minute)
				onUploadProgress(written, total)
			},
			editProgress,
			msgID,
			botToken,
			adminChatID,
			sentMsgID,
			colabSendChatID,
			colabSendAccessHash,
			topicID,
			colabLogChatID,
			colabLogAccessHash,
			finalCaptionForColab,
		)
	} else {
		if colabConfigured && wmOk {
			// Colab configured आहे आणि watermark set आहे, पण worker offline —
			// त्यामुळे तोच watermark process (download → FFmpeg drawtext →
			// reupload) आता main server वरच (CPU) चालवतो. Feature मध्ये काहीच
			// फरक नाही, फक्त encoding location Colab ऐवजी main server.
			editProgress(fmt.Sprintf(
				"⚠️ Watermark server offline — applying watermark on main server\n📄 `%s`\n💾 %s\n📌 Msg: `%d`",
				file.FileName, formatSize(file.FileSize), msgID,
			))
			var tmpPath string
			uploadedFile, tmpPath, err = downloadWatermarkAndReuploadLocal(
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
			if tmpPath != "" {
				pm_tempPath = tmpPath
			}
		} else {
			var tmpPath string
			uploadedFile, tmpPath, err = downloadAndReupload(
				stallCtx,
				upCtx,
				userbotClient,
				botClient,
				file,
				onDownloadProgress,
				onUploadProgress,
			)
			if tmpPath != "" {
				// sendPreparedMessage thumbnail extract karayala use karto,
				// nantar toch delete karnar (defer ethun karta yet nahi kaaran
				// return zhalyavar tempVideoPath caller kade jato)
				pm_tempPath = tmpPath
			}
		}
	}

	stallTimer.Stop()

	if err != nil {
		if pm_tempPath != "" {
			os.Remove(pm_tempPath)
		}
		if stallCtx.Err() != nil && bgCtx.Err() == nil {
			return nil, fmt.Errorf("download stalled (30min no progress) | Retry: /userbotdl %d %d", chatIDRaw, msgID)
		}
		return nil, fmt.Errorf("download/upload failed: %w", err)
	}

	// Colab ne Telegram la already pathavle — Phase B la kahi karayachi garaj nahi
	if colabSent {
		if pm_tempPath != "" {
			os.Remove(pm_tempPath)
		}
		return &preparedMessage{colabSent: true}, nil
	}

	return &preparedMessage{
		tgMsg:         tgMsg,
		finalCaption:  finalCaption,
		file:          file,
		uploadedFile:  uploadedFile,
		msgID:         msgID,
		tempVideoPath: pm_tempPath,
	}, nil
}

// ── sendPreparedMessage ────────────────────────────────────────────────────────
// Phase B: prepareMessage चं output घेऊन Telegram कडे final media send करतो
// (Step 5-8: thumbnail, build media, send to admin/sendChannel/sourcePeer + log channel).
// textOnly / colabSent cases साठी हे काहीच करत नाही (Phase A ने आधीच पूर्ण केलं).

func sendPreparedMessage(
	bgCtx context.Context,
	userbotClient *gotgproto.Client,
	botClient *gotgproto.Client,
	chatID int64,
	chatIDRaw int64,
	topicID int,
	botPeer tg.InputPeerClass,
	sourcePeer *tg.InputPeerChannel,
	sendChannelPeer *tg.InputPeerChannel,
	editProgress func(string),
	pm *preparedMessage,
) error {

	if pm.textOnly || pm.colabSent {
		return nil
	}

	tgMsg := pm.tgMsg
	finalCaption := pm.finalCaption
	file := pm.file
	uploadedFile := pm.uploadedFile
	msgID := pm.msgID

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

		// Fallback 3: setthumbnail nahi, Telegram thumb nahi/fail →
		// pm.tempVideoPath var FFmpeg ne video madhlach frame extract karto.
		// Colab path la he hot nahi (Colab swatah extract karto + colabSent=true
		// aslyamule ha block la reach hoch nahi).
		if len(thumbSrc) == 0 && pm.tempVideoPath != "" {
			thumbPath := pm.tempVideoPath + "_thumb.jpg"
			// Video duration probe
			durOut, _ := exec.Command("ffprobe", "-v", "error",
				"-show_entries", "format=duration",
				"-of", "default=noprint_wrappers=1:nokey=1",
				pm.tempVideoPath).Output()
			var seekSec int = 5
			if d, err := strconv.ParseFloat(strings.TrimSpace(string(durOut)), 64); err == nil && d > 0 {
				seekSec = int(d * 0.2)
				if seekSec < 1 {
					seekSec = 1
				}
			}
			extractErr := exec.Command("ffmpeg",
				"-ss", strconv.Itoa(seekSec),
				"-i", pm.tempVideoPath,
				"-vframes", "1",
				"-q:v", "2",
				"-y", thumbPath).Run()
			if extractErr == nil {
				if data, readErr := os.ReadFile(thumbPath); readErr == nil && len(data) > 0 {
					thumbSrc = data
				}
				os.Remove(thumbPath)
			}
		}
	}

	// tempVideoPath — thumb extract zala (yashswi ki nahi) — cleanup
	if pm.tempVideoPath != "" {
		os.Remove(pm.tempVideoPath)
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
// NEW: Colab polling queue use karto.
// Flow:
//   1. Userbot ne file download karo → Koyeb temp file
//   2. Temp file token generate karo → /api/colab/tempfile/:token serve
//   3. Colab job queue madhe submit karo
//   4. Colab poll → download → FFmpeg → Bot API send
//   5. Koyeb wait karto → done/failed
//   6. Colab ne already Telegram la pathavle → caller ne send karaychi garaj nahi
//      (return nil, true = colabSent, nil)
//   7. Failed → error return (no local fallback)
//
// Returns: (uploadedFile, colabSent bool, error)
// colabSent = true → prepareMessage ne preparedMessage.colabSent=true return karil

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
	botToken string,
	adminChatID int64,
	progressMsgID *int,
	sendToChatID int64,
	sendToAccessHash int64,
	sendToTopicID int,
	logChatID int64,
	logAccessHash int64,
	caption string,
) (tg.InputFileClass, bool, error) {

	// ── Colab Queue: KOYEB_URL set asel tar ───────────────────────────────
	koyebURL := os.Getenv("KOYEB_URL")
	if koyebURL != "" {
		editProgress(fmt.Sprintf(
			"☁️ (Phase 1) Downloading to main server...\n📄 `%s`\n📌 Msg: `%d`",
			file.FileName, msgID,
		))

		// ── Download to local disk first to avoid proxy timeouts (HF 60s timeout) ──
		tmpFile, err := os.CreateTemp("", "colab-pre-*.tmp")
		if err != nil {
			return nil, false, fmt.Errorf("temp file create failed: %w", err)
		}
		tmpPath := tmpFile.Name()
		defer os.Remove(tmpPath)

		var dlWriter io.WriterAt = tmpFile
		if onDownloadProgress != nil {
			dlWriter = &progressWriter{w: tmpFile, total: file.FileSize, onProgress: onDownloadProgress}
		}

		d := downloader.NewDownloader()
		_, err = d.Download(dlClient.API(), file.Location).WithThreads(3).Parallel(dlCtx, dlWriter)
		tmpFile.Close()
		if err != nil {
			return nil, false, fmt.Errorf("pre-download failed: %w", err)
		}

		editProgress(fmt.Sprintf(
			"☁️ Colab la job pathavat aaho...\n📄 `%s`\n📌 Msg: `%d`",
			file.FileName, msgID,
		))

		// Temp file token
		pipeToken := fmt.Sprintf("pipe-%d-%d", time.Now().UnixNano(), rand.Intn(9999))
		
		wmqueue.RegisterTempFile(pipeToken, tmpPath, file.FileName)
		defer wmqueue.UnregisterTempFile(pipeToken)

		fileURL := fmt.Sprintf("%s/api/colab/tempfile/%s", koyebURL, pipeToken)

		jobID, submitErr := wmqueue.SubmitJob(
			fileURL,
			file.FileName,
			file.FileSize,
			wmText,
			botToken,
			adminChatID,
			*progressMsgID,
			sendToChatID,
			sendToAccessHash,
			sendToTopicID,
			logChatID,
			logAccessHash,
			caption,
			msgID,
			false, // protectContent — /userbotdl admin upload: forward block नको
			false, // logOnly — /userbotdl नेहमी normal delivery, silent pre-download नाही
			0,     // channelID — 0 = जुना pipe/file_url mode (unchanged)
			0,     // channelAccessHash — pipe mode मध्ये लागत नाही
			"",    // downloadBotToken — रिकामा = जुना pipe/file_url mode (unchanged)
		)
		if submitErr == nil {
			editProgress(fmt.Sprintf(
				"☁️ Job submitted! Colab processing...\n📄 `%s`\n🆔 `%s`\n📌 Msg: `%d`",
				file.FileName, jobID, msgID,
			))

			// WaitForDone साठी upCtx (30min timeout) नको — Colab बराच वेळ
			// offline राहिला तरी job wmqueue मध्ये जिवंत राहतो आणि परत
			// online झाल्यावर resume होतो. फक्त bot/process shutdown
			// (bgCtx-कार्यान्वित) झाल्यास थांबावं.
			success, jobErr, _, _ := wmqueue.WaitForDone(context.Background(), jobID)
			if success {
				// Colab ne FFmpeg karun, Telethon (bot_token, MTProto) ne
				// directly target chat la file pathavli — Koyeb la file
				// kadhich milat nahi, fakt status milto.
				return nil, true, nil
			}

			// Colab failed — no local fallback
			errMsg := jobErr
			if errMsg == "" {
				errMsg = "colab job failed (no error details)"
			}
			editProgress(fmt.Sprintf(
				"❌ Colab failed\n📄 `%s`\n📌 Msg: `%d`\n`%s`",
				file.FileName, msgID, errMsg,
			))
			return nil, false, fmt.Errorf("colab watermark failed: %s", errMsg)
		}
	}

	// KOYEB_URL set nahi — Colab unavailable
	return nil, false, fmt.Errorf("colab unavailable: KOYEB_URL not set")
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
) (tg.InputFileClass, string, error) {

	tmpFile, err := os.CreateTemp("", "userbotdl-*.tmp")
	if err != nil {
		return nil, "", fmt.Errorf("temp file create failed: %w", err)
	}
	tmpPath := tmpFile.Name()
	// delete caller karnar — thumb extract sathi lagel
	// (defer os.Remove kadhun takla)

	var dlWriter io.WriterAt = tmpFile
	if onDownloadProgress != nil {
		dlWriter = &progressWriter{
			w:          tmpFile,
			total:      file.FileSize,
			onProgress: onDownloadProgress,
		}
	}

	d := downloader.NewDownloader()
	_, err = d.Download(dlClient.API(), file.Location).WithThreads(3).Parallel(dlCtx, dlWriter)
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("download failed: %w", err)
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("temp file stat failed: %w", err)
	}
	totalSize := info.Size()

	readFile, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("temp file reopen failed: %w", err)
	}
	defer readFile.Close()

	var upProgress uploader.Progress
	if onUploadProgress != nil {
		upProgress = &uploadProgressAdapter{onProgress: onUploadProgress}
	}

	up := uploader.NewUploader(upClient.API()).WithPartSize(256 * 1024).WithThreads(3)
	if upProgress != nil {
		up = up.WithProgress(upProgress)
	}
	upload := uploader.NewUpload(file.FileName, readFile, totalSize)
	uploadedFile, err := up.Upload(upCtx, upload)
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("upload failed: %w", err)
	}

	return uploadedFile, tmpPath, nil
}

// ── downloadWatermarkAndReuploadLocal ─────────────────────────────────────────
// [FALLBACK] Colab/watermark worker offline असताना तोच process (download →
// FFmpeg drawtext watermark overlay → reupload) main server वरच replicate
// करतो — जेणेकरून खालचा Phase B (thumbnail extract, sendChannel, sourcePeer,
// log channel backup) कुठलाही बदल न करता जसाच्या तसा चालतो, फक्त encoding आता
// Colab GPU ऐवजी main server च्या CPU वर होतं (libx264 ultrafast — Colab worker
// चाच tier3 CPU fallback वापरतो, त्यामुळे quality/behaviour सारखंच राहतं).
func downloadWatermarkAndReuploadLocal(
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
) (tg.InputFileClass, string, error) {

	// ── Step 1: Download (Colab worker सारखंच पहिलं full download) ─────────
	tmpFile, err := os.CreateTemp("", "userbotdl-in-*.tmp")
	if err != nil {
		return nil, "", fmt.Errorf("temp file create failed: %w", err)
	}
	inputPath := tmpFile.Name()

	var dlWriter io.WriterAt = tmpFile
	if onDownloadProgress != nil {
		dlWriter = &progressWriter{w: tmpFile, total: file.FileSize, onProgress: onDownloadProgress}
	}

	d := downloader.NewDownloader()
	_, err = d.Download(dlClient.API(), file.Location).WithThreads(3).Parallel(dlCtx, dlWriter)
	tmpFile.Close()
	if err != nil {
		os.Remove(inputPath)
		return nil, "", fmt.Errorf("download failed: %w", err)
	}

	// ── Step 2: FFmpeg drawtext watermark overlay ───────────────────────────
	ext := filepath.Ext(file.FileName)
	if ext == "" {
		ext = ".mp4"
	}
	outFile, err := os.CreateTemp("", "userbotdl-wm-*"+ext)
	if err != nil {
		os.Remove(inputPath)
		return nil, "", fmt.Errorf("output temp file create failed: %w", err)
	}
	outputPath := outFile.Name()
	outFile.Close()
	os.Remove(outputPath) // ffmpeg -y स्वतः तयार करेल — फक्त unique नाव हवं होतं

	if err := runLocalWatermarkFFmpeg(inputPath, outputPath, wmText, editProgress, file.FileName, msgID); err != nil {
		os.Remove(inputPath)
		os.Remove(outputPath)
		return nil, "", fmt.Errorf("ffmpeg watermark failed: %w", err)
	}
	os.Remove(inputPath) // watermark झालेली output file आता वापरायची, input नको

	// ── Step 3: Upload processed file (downloadAndReupload सारखंच) ─────────
	info, err := os.Stat(outputPath)
	if err != nil {
		os.Remove(outputPath)
		return nil, "", fmt.Errorf("output file stat failed: %w", err)
	}
	totalSize := info.Size()

	readFile, err := os.Open(outputPath)
	if err != nil {
		os.Remove(outputPath)
		return nil, "", fmt.Errorf("output file reopen failed: %w", err)
	}
	defer readFile.Close()

	var upProgress uploader.Progress
	if onUploadProgress != nil {
		upProgress = &uploadProgressAdapter{onProgress: onUploadProgress}
	}

	up := uploader.NewUploader(upClient.API()).WithPartSize(256 * 1024).WithThreads(3)
	if upProgress != nil {
		up = up.WithProgress(upProgress)
	}
	upload := uploader.NewUpload(file.FileName, readFile, totalSize)
	uploadedFile, err := up.Upload(upCtx, upload)
	if err != nil {
		os.Remove(outputPath)
		return nil, "", fmt.Errorf("upload failed: %w", err)
	}

	// caller (sendPreparedMessage) outputPath ला thumbnail extract sathi
	// vaparto, mag delete karto — downloadAndReupload sarkhach contract.
	return uploadedFile, outputPath, nil
}

// ── runLocalWatermarkFFmpeg ────────────────────────────────────────────────
// Colab worker च्याच drawtext style ने (bottom-right corner, white text +
// black shadow) CPU वर (libx264 ultrafast, crf 28) watermark overlay करतो.
// ffmpeg "-progress pipe:1" मधून periodic % editProgress ला पाठवतो, जेणेकरून
// admin ला "💧 Watermark applying..." हा Colab सारखाच feel येतो.
func runLocalWatermarkFFmpeg(inputPath, outputPath, wmText string, editProgress func(string), fileName string, msgID int) error {
	totalDur := getLocalVideoDuration(inputPath)

	safeText := strings.ReplaceAll(wmText, `'`, `\'`)
	safeText = strings.ReplaceAll(safeText, `:`, `\:`)
	drawtext := fmt.Sprintf(
		"drawtext=text='%s':fontcolor=white:fontsize=36:shadowcolor=black:shadowx=2:shadowy=2:x=w-tw-20:y=h-th-20",
		safeText,
	)

	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-vf", drawtext,
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28",
		"-threads", "0", "-tune", "fastdecode",
		"-c:a", "copy", "-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		"-y", outputPath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe failed: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start failed: %w", err)
	}

	start := time.Now()
	lastUpdate := start
	var curSeconds float64

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_ms=") {
			if v, perr := strconv.ParseInt(strings.TrimPrefix(line, "out_time_ms="), 10, 64); perr == nil && v > 0 {
				curSeconds = float64(v) / 1_000_000
			}
		}
		if editProgress != nil && time.Since(lastUpdate) >= 4*time.Second {
			elapsed := time.Since(start).Round(time.Second)
			if totalDur > 0 && curSeconds > 0 {
				pct := curSeconds / totalDur * 100
				if pct > 99 {
					pct = 99
				}
				var eta time.Duration
				if pct > 0 {
					eta = time.Duration(elapsed.Seconds() / pct * (100 - pct) * float64(time.Second))
				}
				editProgress(fmt.Sprintf(
					"💧 Watermark applying (main server)...STEP.2/3\n📄 `%s`\n📌 Msg: `%d`\n%s %.1f%%\n⏱ %s | ETA: %s",
					fileName, msgID, progressBar(int(pct)), pct, elapsed, eta.Round(time.Second),
				))
			} else {
				editProgress(fmt.Sprintf(
					"💧 Watermark applying (main server)...STEP.2/3\n📄 `%s`\n📌 Msg: `%d`\n⏱ %s elapsed...",
					fileName, msgID, elapsed,
				))
			}
			lastUpdate = time.Now()
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg exit error: %w", err)
	}

	if info, statErr := os.Stat(outputPath); statErr != nil || info.Size() == 0 {
		return fmt.Errorf("ffmpeg produced empty/missing output")
	}

	return nil
}

// ── getLocalVideoDuration ──────────────────────────────────────────────────
// ffprobe ने video duration (seconds) काढतो — ETA calculation साठी. Fail
// झाल्यास 0 return करतो (caller ETA न दाखवता फक्त elapsed time दाखवतो).
func getLocalVideoDuration(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path,
	).Output()
	if err != nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return v
}

// ── progressWriter ────────────────────────────────────────────────────────────

type progressWriter struct {
	w          io.Writer
	total      int64
	written    int64
	lastPct    int64
	onProgress func(written, total int64)
	mu         sync.Mutex // Added for thread-safety in WriteAt
	lastTime   time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.mu.Lock()
	p.written += int64(n)
	if p.total > 0 {
		pct := p.written * 100 / p.total
		if pct >= p.lastPct+1 || p.written >= p.total {
			p.lastPct = pct
			if p.onProgress != nil {
				p.onProgress(p.written, p.total)
			}
		}
	}
	p.mu.Unlock()
	return n, err
}

func (p *progressWriter) WriteAt(b []byte, off int64) (int, error) {
	wAt, ok := p.w.(io.WriterAt)
	if !ok {
		return 0, fmt.Errorf("underlying writer does not implement WriterAt")
	}
	n, err := wAt.WriteAt(b, off)
	p.mu.Lock()
	p.written += int64(n)
	if p.total > 0 {
		pct := p.written * 100 / p.total
		if pct >= p.lastPct+1 || p.written >= p.total {
			p.lastPct = pct
			if p.onProgress != nil {
				p.onProgress(p.written, p.total)
			}
		}
	}
	p.mu.Unlock()
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
		if pct >= p.lastPct+1 || p.written >= p.total {
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

type uploadProgressAdapter struct {
	onProgress func(written, total int64)
	lastPct    int64
	mu         sync.Mutex
}


func (p *uploadProgressAdapter) Chunk(ctx context.Context, state uploader.ProgressState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state.Total > 0 {
		pct := state.Uploaded * 100 / state.Total
		if pct >= p.lastPct+1 || state.Uploaded >= state.Total {
			p.lastPct = pct
			if p.onProgress != nil {
				p.onProgress(state.Uploaded, state.Total)
			}
		}
	}
	return nil
}

