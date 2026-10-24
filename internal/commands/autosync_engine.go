// internal/commands/autosync_engine.go
//
// AutoSync engine — source group/channel madhun navin messages destination
// channel la scheduled (daily, multiple times) copy karto.
//
//   * Userbot account per task select hoto (system userbot kiva bot madhun add kelele extra accounts).
//   * Mode "fast": media reference ne copy (download nahi) — fail zalyas auto "full".
//     Mode "full": userbot download + re-upload (protected/no-forward channels sathi pan chalto).
//   * Caption/text var replace rules lagtat: global admin rules + task che swatache rules.
//   * Source forum asel ani mirror ON asel tar destination madhe same topic aapoaap banto.
//   * Duplicate nahi: source cursor (last_msg_id) + (task, src msg) unique map + deterministic RandomID.
package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson"
	"go.uber.org/zap"
)

// asSlotOwnerBase — bot madhun add kelelya extra userbot accounts sathi synthetic owner ID base.
// (session "user_<base+n>" mhanun save hoto; real Telegram IDs peksha khup motha, collide hot nahi)
const asSlotOwnerBase int64 = 7_000_000_000_000

var (
	asRunning sync.Map                // task hex ID -> true (ekach veli ek run)
	asSem     = make(chan struct{}, 2) // ek veli max 2 tasks
	asLog     *zap.Logger
)

// ─── userbot client ──────────────────────────────────────────────────────────

func asClientForKey(ctx context.Context, key string) (*gotgproto.Client, error) {
	if strings.HasPrefix(key, "user_") {
		id, err := strconv.ParseInt(strings.TrimPrefix(key, "user_"), 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("invalid userbot session key: %s", key)
		}
		return reviveUserbot(ctx, id)
	}
	// system (main) userbot
	for i := 0; i < 3; i++ {
		var c *gotgproto.Client
		if GetLiveUserbotClient != nil {
			c = GetLiveUserbotClient()
		}
		if c == nil && GetUserbotClient != nil {
			c = GetUserbotClient()
		}
		if c != nil && pingUserbot(ctx, c) {
			return c, nil
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("system userbot chalu nahi (login kara)")
}

// ─── chats ───────────────────────────────────────────────────────────────────

type asChat struct {
	ID        int64 // Bot-API style: -100xxxxxxxxxx
	Title     string
	Forum     bool
	Broadcast bool
	CanPost   bool
	Input     *tg.InputChannel
	Peer      *tg.InputPeerChannel
}

func asFromChannel(c *tg.Channel) *asChat {
	_, hasAdmin := c.GetAdminRights()
	return &asChat{
		ID:        -1000000000000 - c.ID,
		Title:     c.Title,
		Forum:     c.Forum,
		Broadcast: c.Broadcast,
		CanPost:   c.Creator || hasAdmin || !c.Broadcast,
		Input:     &tg.InputChannel{ChannelID: c.ID, AccessHash: c.AccessHash},
		Peer:      &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash},
	}
}

// asListDialogs — userbot che joined channels / supergroups (recent 100 dialogs).
func asListDialogs(ctx context.Context, c *gotgproto.Client) ([]*asChat, error) {
	res, err := utils.ExecuteWithFloodWait(ctx, func() (tg.MessagesDialogsClass, error) {
		return c.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      100,
		})
	})
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, errors.New("dialogs parse zale nahit")
	}
	var out []*asChat
	for _, chat := range mod.GetChats() {
		if cc, ok := chat.(*tg.Channel); ok && !cc.Left {
			out = append(out, asFromChannel(cc))
		}
	}
	return out, nil
}

// asResolveChat — Bot-API style chatID (-100...) varun fresh access hash sah chat.
func asResolveChat(ctx context.Context, c *gotgproto.Client, chatID int64) (*asChat, error) {
	mt := chatID
	if mt < 0 {
		mt = (-mt) - 1000000000000
	}
	if in, err := utils.GetChannelPeer(ctx, c.API(), c.PeerStorage, chatID); err == nil && in != nil {
		if chats, err2 := c.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{in}); err2 == nil {
			for _, chat := range chats.GetChats() {
				if cc, ok := chat.(*tg.Channel); ok && cc.ID == mt {
					return asFromChannel(cc), nil
				}
			}
		}
	}
	if list, err := asListDialogs(ctx, c); err == nil {
		for _, ac := range list {
			if ac.ID == chatID {
				return ac, nil
			}
		}
	}
	return nil, fmt.Errorf("chat %d userbot la sapadla nahi — userbot tya group/channel cha member ahe ka?", chatID)
}

// ─── source history ──────────────────────────────────────────────────────────

// asFetchNew — minID peksha motha sagle messages (ascending). maxSeen = sarvat motha id (service sahit).
func asFetchNew(ctx context.Context, api *tg.Client, peer *tg.InputPeerChannel, minID int) ([]*tg.Message, int, error) {
	var all []*tg.Message
	maxSeen := minID
	offsetID := 0
	for page := 0; page < 500; page++ {
		oid := offsetID
		res, err := utils.ExecuteWithFloodWait(ctx, func() (tg.MessagesMessagesClass, error) {
			return api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     peer,
				OffsetID: oid,
				Limit:    100,
				MinID:    minID,
			})
		})
		if err != nil {
			return nil, 0, err
		}
		var msgs []tg.MessageClass
		switch r := res.(type) {
		case *tg.MessagesChannelMessages:
			msgs = r.Messages
		case *tg.MessagesMessagesSlice:
			msgs = r.Messages
		case *tg.MessagesMessages:
			msgs = r.Messages
		}
		if len(msgs) == 0 {
			break
		}
		lowest := 0
		for _, mc := range msgs {
			id := mc.GetID()
			if id > maxSeen {
				maxSeen = id
			}
			if lowest == 0 || id < lowest {
				lowest = id
			}
			if m, ok := mc.(*tg.Message); ok && m.ID > minID {
				all = append(all, m)
			}
		}
		if len(msgs) < 100 || lowest <= minID+1 || lowest == offsetID {
			break
		}
		offsetID = lowest
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, maxSeen, nil
}

// asLatestID — source madhla sadhyacha shevtacha message id (0 = rikama).
func asLatestID(ctx context.Context, api *tg.Client, peer *tg.InputPeerChannel) int {
	res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 1})
	if err != nil {
		return 0
	}
	var msgs []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesChannelMessages:
		msgs = r.Messages
	case *tg.MessagesMessagesSlice:
		msgs = r.Messages
	case *tg.MessagesMessages:
		msgs = r.Messages
	}
	top := 0
	for _, mc := range msgs {
		if id := mc.GetID(); id > top {
			top = id
		}
	}
	return top
}

// asMsgTopic — message konatya forum topic madhe ahe (0 = General / forum nahi).
func asMsgTopic(m *tg.Message) int {
	h, ok := m.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || !h.ForumTopic {
		return 0
	}
	if h.ReplyToTopID != 0 {
		return h.ReplyToTopID
	}
	return h.ReplyToMsgID
}

// asRealReplyID — message khara kuthlya message la reply ahe (topic root nahi) tar tyacha id.
func asRealReplyID(m *tg.Message) int {
	h, ok := m.ReplyTo.(*tg.MessageReplyHeader)
	if !ok {
		return 0
	}
	if !h.ForumTopic {
		return h.ReplyToMsgID
	}
	if h.ReplyToTopID != 0 {
		return h.ReplyToMsgID
	}
	return 0
}

// ─── destination topics ──────────────────────────────────────────────────────

func asMsgIDFromUpdates(upd tg.UpdatesClass) int {
	scan := func(list []tg.UpdateClass) int {
		for _, u := range list {
			switch v := u.(type) {
			case *tg.UpdateNewChannelMessage:
				switch m := v.Message.(type) {
				case *tg.Message:
					return m.ID
				case *tg.MessageService:
					return m.ID
				}
			case *tg.UpdateNewMessage:
				switch m := v.Message.(type) {
				case *tg.Message:
					return m.ID
				case *tg.MessageService:
					return m.ID
				}
			}
		}
		for _, u := range list {
			if v, ok := u.(*tg.UpdateMessageID); ok {
				return v.ID
			}
		}
		return 0
	}
	switch r := upd.(type) {
	case *tg.Updates:
		return scan(r.Updates)
	case *tg.UpdatesCombined:
		return scan(r.Updates)
	case *tg.UpdateShortSentMessage:
		return r.ID
	}
	return 0
}

// asEnsureTopic — source topic sathi destination madhe topic (nasel tar banavto, title copy karto).
func asEnsureTopic(ctx context.Context, api *tg.Client, t *db.AutoSyncTask, src, dst *asChat, srcTopic int) (int, error) {
	if srcTopic <= 1 {
		return 0, nil // General topic
	}
	if d, ok := db.AutoSyncTopicGet(t.ID, srcTopic); ok {
		return d, nil
	}
	title := fmt.Sprintf("Topic %d", srcTopic)
	if res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: src.Input,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: srcTopic}},
	}); err == nil {
		if cm, ok := res.(*tg.MessagesChannelMessages); ok {
			for _, mc := range cm.Messages {
				if ms, ok := mc.(*tg.MessageService); ok {
					if a, ok := ms.Action.(*tg.MessageActionTopicCreate); ok && strings.TrimSpace(a.Title) != "" {
						title = a.Title
					}
				}
			}
		}
	}
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120])
	}
	created, err := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
		return api.MessagesCreateForumTopic(ctx, &tg.MessagesCreateForumTopicRequest{
			Peer:     dst.Peer,
			Title:    title,
			RandomID: deterministicRandomID("astopic", t.ID.Hex(), strconv.Itoa(srcTopic)),
		})
	})
	if err != nil {
		return 0, fmt.Errorf("topic create fail: %w", err)
	}
	id := asMsgIDFromUpdates(created)
	if id <= 0 {
		return 0, errors.New("navin topic cha id milala nahi")
	}
	_ = db.AutoSyncTopicPut(t.ID, srcTopic, id)
	return id, nil
}

// ─── send one message ────────────────────────────────────────────────────────

var errAsUnsupported = errors.New("unsupported message type (poll/game/etc) — skip")

func asSendOne(ctx context.Context, c *gotgproto.Client, t *db.AutoSyncTask, dst *asChat, m *tg.Message, destTopic, replyDest int) (int, error) {
	api := c.API()
	tid := t.ID.Hex()

	text := m.Message
	newText := applyRules(applyAdminReplaces(text), t.Replaces)
	var entities []tg.MessageEntityClass
	if newText == text {
		entities = m.Entities
	}

	var replyTo tg.InputReplyToClass
	if replyDest > 0 {
		replyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyDest}
	} else if destTopic > 1 {
		replyTo = &tg.InputReplyToMessage{ReplyToMsgID: destTopic}
	}
	rid := func(part string) int64 {
		return deterministicRandomID("autosync", t.ID.Hex(), strconv.Itoa(m.ID), part)
	}

	sendText := func(txt string, ents []tg.MessageEntityClass, part string) (int, error) {
		upd, err := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
			return api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     dst.Peer,
				Message:  txt,
				Entities: ents,
				ReplyTo:  replyTo,
				RandomID: rid(part),
			})
		})
		if err != nil {
			return 0, err
		}
		return asMsgIDFromUpdates(upd), nil
	}

	// ── media nasel / webpage preview — text ─────────────────────────────────
	var media tg.MessageMediaClass = m.Media
	switch media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		if strings.TrimSpace(newText) == "" {
			return 0, errAsUnsupported
		}
		return sendText(newText, entities, "txt")
	case *tg.MessageMediaPhoto, *tg.MessageMediaDocument:
		// niche
	default:
		if strings.TrimSpace(newText) == "" {
			return 0, errAsUnsupported
		}
		return sendText(newText, entities, "txt")
	}

	// ── media + caption ──────────────────────────────────────────────────────
	caption, capEnts := newText, entities
	var followText string
	var followEnts []tg.MessageEntityClass
	if len([]rune(caption)) > 1024 { // non-premium caption limit — media gela, text nantar
		followText, followEnts = caption, capEnts
		caption, capEnts = "", nil
	}

	buildFast := func() (tg.InputMediaClass, error) {
		switch md := media.(type) {
		case *tg.MessageMediaPhoto:
			ph, ok := md.Photo.AsNotEmpty()
			if !ok {
				return nil, errors.New("photo empty")
			}
			return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: ph.ID, AccessHash: ph.AccessHash, FileReference: ph.FileReference}}, nil
		case *tg.MessageMediaDocument:
			d, ok := md.Document.AsNotEmpty()
			if !ok {
				return nil, errors.New("document empty")
			}
			return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}, nil
		}
		return nil, errAsUnsupported
	}

	buildFull := func() (tg.InputMediaClass, func(), error) {
		cleanup := func() {}
		switch md := media.(type) {
		case *tg.MessageMediaPhoto:
			f, err := utils.FileFromMedia(md)
			if err != nil {
				return nil, cleanup, err
			}
			asProgSub(tid, "⬇️ Download (photo)", 0, 0)
			var buf bytes.Buffer
			if _, err := downloader.NewDownloader().Download(api, f.Location).Stream(ctx, &buf); err != nil {
				return nil, cleanup, fmt.Errorf("photo download fail: %w", err)
			}
			asProgSub(tid, "⬆️ Upload (photo)", 0, 0)
			up, err := uploader.NewUploader(api).FromBytes(ctx, f.FileName, buf.Bytes())
			if err != nil {
				return nil, cleanup, fmt.Errorf("photo upload fail: %w", err)
			}
			return &tg.InputMediaUploadedPhoto{File: up}, cleanup, nil
		case *tg.MessageMediaDocument:
			d, ok := md.Document.AsNotEmpty()
			if !ok {
				return nil, cleanup, errors.New("document empty")
			}
			f, err := utils.FileFromMedia(md)
			if err != nil {
				return nil, cleanup, err
			}
			onDl := func(w, tot int64) { asProgSub(tid, "⬇️ Download", w, tot) }
			onUp := func(w, tot int64) { asProgSub(tid, "⬆️ Upload", w, tot) }
			upFile, tmpPath, err := downloadAndReupload(ctx, ctx, c, c, f, onDl, onUp)
			if tmpPath != "" {
				cleanup = func() { _ = os.Remove(tmpPath) }
			}
			if err != nil {
				return nil, cleanup, err
			}
			return &tg.InputMediaUploadedDocument{File: upFile, MimeType: d.MimeType, Attributes: d.Attributes}, cleanup, nil
		}
		return nil, cleanup, errAsUnsupported
	}

	sendMedia := func(im tg.InputMediaClass) (int, error) {
		asProgSub(tid, "📤 Telegram la pathvat ahe", 0, 0)
		upd, err := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
			return api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:     dst.Peer,
				Media:    im,
				Message:  caption,
				Entities: capEnts,
				ReplyTo:  replyTo,
				RandomID: rid("media"),
			})
		})
		if err != nil {
			return 0, err
		}
		return asMsgIDFromUpdates(upd), nil
	}

	var destID int
	var sendErr error
	tryFull := t.Mode == "full"

	if !tryFull { // fast
		asProgSub(tid, "⚡ Fast copy", 0, 0)
		im, err := buildFast()
		if err == nil {
			destID, sendErr = sendMedia(im)
		} else {
			sendErr = err
		}
		if sendErr != nil {
			if ctx.Err() != nil || isTransientConnErr(sendErr) {
				return 0, sendErr
			}
			tryFull = true // fast fail → full fallback
		}
	}
	if tryFull {
		im, cleanup, err := buildFull()
		defer cleanup()
		if err != nil {
			return 0, err
		}
		destID, sendErr = sendMedia(im)
		if sendErr != nil {
			return 0, sendErr
		}
	}

	if followText != "" {
		if _, err := sendText(followText, followEnts, "cap"); err != nil {
			return destID, nil // media gela — caption text fail zala tari message "copied"
		}
	}
	return destID, nil
}

// asIsFatalErr — systemic error (permission / ban / dead session / flood): message skip karun
// pudhe jaycha nahi — run thambvaycha, admin la hint dyaycha ani message GELE NAHIT (cursor tasach).
func asIsFatalErr(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	up := strings.ToUpper(err.Error())
	for _, h := range []struct{ code, hint string }{
		{"CHAT_ADMIN_REQUIRED", "Userbot la destination madhe ADMIN right dya (Post Messages + Manage Topics)."},
		{"CHAT_WRITE_FORBIDDEN", "Userbot la destination madhe post karnyachi parvangi nahi."},
		{"CHAT_SEND_PLAIN_FORBIDDEN", "Destination madhe text pathvnyachi parvangi nahi."},
		{"CHAT_SEND_MEDIA_FORBIDDEN", "Destination madhe media pathvnyachi parvangi nahi."},
		{"CHAT_SEND_PHOTOS_FORBIDDEN", "Destination madhe photos pathvnyachi parvangi nahi."},
		{"CHAT_SEND_VIDEOS_FORBIDDEN", "Destination madhe videos pathvnyachi parvangi nahi."},
		{"CHAT_SEND_DOCS_FORBIDDEN", "Destination madhe files pathvnyachi parvangi nahi."},
		{"USER_BANNED_IN_CHANNEL", "Userbot destination madhun ban ahe."},
		{"CHANNEL_PRIVATE", "Userbot la chat cha access nahi (left / banned)."},
		{"CHANNEL_INVALID", "Chat access invalid — userbot tya chat madhe ahe ka?"},
		{"CHAT_RESTRICTED", "Chat restricted ahe."},
		{"TOPIC_CLOSED", "Destination topic CLOSED ahe — topic reopen kara."},
		{"CHANNEL_FORUM_MISSING", "Destination forum nahi (Topics ON kara)."},
		{"FORUM_ENABLED", "Destination forum setting cha problem — Topics setting tapasa."},
		{"AUTH_KEY_UNREGISTERED", "Userbot session expire zala — parat login kara."},
		{"SESSION_REVOKED", "Userbot session revoke zala — parat login kara."},
		{"USER_DEACTIVATED", "Userbot account deactivate/ban ahe."},
	} {
		if strings.Contains(up, h.code) {
			return true, h.hint
		}
	}
	if sec, ok := utils.ExtractFloodWaitSeconds(err); ok {
		return true, fmt.Sprintf("Telegram flood limit (%d sec wait) — pudhchya run madhe continue hoil.", sec)
	}
	return false, ""
}

func asIsTopicGoneErr(err error) bool {
	if err == nil {
		return false
	}
	up := strings.ToUpper(err.Error())
	return strings.Contains(up, "TOPIC_DELETED") || strings.Contains(up, "TOPIC_ID_INVALID")
}

// ─── run one task ────────────────────────────────────────────────────────────

func asMaxPerRun() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AUTOSYNC_MAX_PER_RUN"))); err == nil && v > 0 {
		return v
	}
	return 2000
}

func asFinish(t *db.AutoSyncTask, status string, copied int, notify bool) string {
	asProgFinish(t.ID.Hex())
	_ = db.SetAutoSyncFields(t.ID, bson.M{"last_run_at": time.Now(), "last_status": status})
	if copied > 0 {
		_ = db.IncAutoSyncCopied(t.ID, int64(copied))
	}
	line := fmt.Sprintf("🔁 AutoSync “%s”\n%s", t.Name, status)
	if notify && NotifyAdmins != nil {
		NotifyAdmins(line)
	}
	return line
}

// runAutoSyncTask — ek task chalavto. manual=true → admin la nehmi report.
func runAutoSyncTask(parent context.Context, taskID string, manual bool) string {
	oid, err := primitiveObjectID(taskID)
	if err != nil {
		return "❌ invalid task id"
	}
	if _, loaded := asRunning.LoadOrStore(taskID, true); loaded {
		return "⏳ Ha task adhich chalu ahe."
	}
	defer asRunning.Delete(taskID)

	select {
	case asSem <- struct{}{}:
	case <-parent.Done():
		return "🛑 stopped"
	}
	defer func() { <-asSem }()

	t, err := db.GetAutoSyncTask(oid)
	if err != nil || t == nil {
		return "❌ Task sapadla nahi (delete zala asel)."
	}

	ctx, cancel := context.WithTimeout(parent, 6*time.Hour)
	defer cancel()

	asProgStart(taskID, t.Name, t.Mode)
	defer asProgFinish(taskID)

	client, err := asClientForKey(ctx, t.SessionKey)
	if err != nil {
		return asFinish(t, "❌ Userbot problem: "+err.Error(), 0, true)
	}
	src, err := asResolveChat(ctx, client, t.SourceChat)
	if err != nil {
		return asFinish(t, "❌ Source: "+err.Error(), 0, true)
	}
	dst, err := asResolveChat(ctx, client, t.DestChat)
	if err != nil {
		return asFinish(t, "❌ Destination: "+err.Error(), 0, true)
	}

	msgs, maxSeen, err := asFetchNew(ctx, client.API(), src.Peer, t.LastMsgID)
	if err != nil {
		return asFinish(t, "❌ Source history fetch fail: "+err.Error(), 0, true)
	}

	maxPerRun := asMaxPerRun()
	mirror := t.MirrorTopics && src.Forum && dst.Forum

	copied, skipped, errCount, remaining := 0, 0, 0, 0
	{
		total, beyond := len(msgs), 0
		if total > maxPerRun {
			total, beyond = maxPerRun, total-maxPerRun
		}
		asProgUpdate(taskID, func(p *asProgress) {
			p.Total, p.Beyond, p.Phase = total, beyond, "📨 Messages pathvat ahe"
		})
	}
	var lastErr string
	completed := true
	processed := 0

	// firstFail — pehla message jyacha send fail zala; cursor tyachya pudhe jat nahi, mhanje
	// to pudhchya run madhe parat try hoto (3 run nantar sodun dila jato). Message HARVAT NAHI.
	firstFail := 0
	var gaveUp []int
	var fatalHint string
	advance := func(id int) {
		if firstFail == 0 {
			_ = db.SetAutoSyncFields(t.ID, bson.M{"last_msg_id": id})
		}
	}

	for idx, m := range msgs {
		if ctx.Err() != nil {
			completed = false
			break
		}
		if processed >= maxPerRun {
			remaining = len(msgs) - idx
			completed = false
			break
		}
		processed++
		asProgUpdate(taskID, func(p *asProgress) {
			p.Done, p.Copied, p.Skipped, p.Errors = processed-1, copied, skipped, errCount
			p.CurMsgID, p.CurDesc = m.ID, asDescribeMsg(m)
			p.Sub, p.SubWritten, p.SubTotal = "", 0, 0
		})

		// ── duplicate guard ──
		if _, done := db.AutoSyncMapGet(t.ID, m.ID); done {
			skipped++
			advance(m.ID)
			continue
		}

		// ── topic ──
		destTopic := 0
		srcTopic := 0
		if mirror {
			srcTopic = asMsgTopic(m)
			if srcTopic > 1 {
				dt, terr := asEnsureTopic(ctx, client.API(), t, src, dst, srcTopic)
				if terr != nil {
					// Topic nasel tar message chukichya thikani takaycha nahi, ani skip pan karaycha nahi —
					// run thambvaycha. Fix (rights / forum setting) nantar tithunach continue hoil.
					completed = false
					errCount++
					lastErr = terr.Error()
					if ctx.Err() == nil && !isTransientConnErr(terr) {
						if _, h := asIsFatalErr(terr); h != "" {
							fatalHint = h
						} else {
							fatalHint = "Destination madhe topic banvta ala nahi — userbot la 'Manage Topics' admin right ahe ka ani destination khara FORUM ahe ka?"
						}
					}
					break
				}
				destTopic = dt
			}
		}

		// ── reply mapping ──
		replyDest := 0
		if rid := asRealReplyID(m); rid > 0 {
			if d, ok := db.AutoSyncMapGet(t.ID, rid); ok && d > 0 {
				replyDest = d
			}
		}

		// ── send (retry) ──
		var destID int
		var sendErr error
		aborted := false
		for attempt := 1; attempt <= 3; attempt++ {
			destID, sendErr = asSendOne(ctx, client, t, dst, m, destTopic, replyDest)
			if sendErr == nil || ctx.Err() != nil || errors.Is(sendErr, errAsUnsupported) {
				break
			}
			if isTransientConnErr(sendErr) {
				aborted = true
				break
			}
			if fatal, hint := asIsFatalErr(sendErr); fatal {
				fatalHint = hint
				aborted = true
				break
			}
			// destination topic delete zala asel — mapping kadhun topic parat banva, mag retry
			if mirror && destTopic > 0 && asIsTopicGoneErr(sendErr) {
				db.AutoSyncTopicDelete(t.ID, srcTopic)
				if dt, terr := asEnsureTopic(ctx, client.API(), t, src, dst, srcTopic); terr == nil {
					destTopic = dt
					continue
				}
			}
			select {
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			case <-ctx.Done():
			}
		}
		if aborted || ctx.Err() != nil {
			completed = false
			if sendErr != nil {
				lastErr = sendErr.Error()
				if fatalHint != "" {
					errCount++
				}
			}
			break
		}
		if sendErr != nil {
			if errors.Is(sendErr, errAsUnsupported) {
				skipped++
				_ = db.AutoSyncMapPut(t.ID, m.ID, 0)
				advance(m.ID)
				continue
			}
			errCount++
			lastErr = fmt.Sprintf("msg %d: %v", m.ID, sendErr)
			if db.AutoSyncFailInc(t.ID, m.ID) >= 3 {
				// 3 vela (3 runs) fail — ha message sodun dyaycha, pudhche chalu theva
				gaveUp = append(gaveUp, m.ID)
				_ = db.AutoSyncMapPut(t.ID, m.ID, 0)
				advance(m.ID)
			} else if firstFail == 0 {
				firstFail = m.ID
			}
			continue
		}

		if destID <= 0 {
			destID = -1
		}
		_ = db.AutoSyncMapPut(t.ID, m.ID, destID)
		advance(m.ID)
		copied++

		if t.Mode != "full" {
			select {
			case <-time.After(1200 * time.Millisecond): // flood avoid
			case <-ctx.Done():
			}
		}
	}

	asProgUpdate(taskID, func(p *asProgress) {
		p.Done, p.Copied, p.Skipped, p.Errors = processed, copied, skipped, errCount
		p.CurDesc, p.Sub = "", ""
	})

	// sagle messages (service sahit) pahile — cursor pudhe nya
	if completed && maxSeen > t.LastMsgID && firstFail == 0 {
		_ = db.SetAutoSyncFields(t.ID, bson.M{"last_msg_id": maxSeen})
	}

	status := fmt.Sprintf("✅ Copied: %d | ⏭ Skipped: %d | ❌ Errors: %d", copied, skipped, errCount)
	if remaining > 0 {
		status += fmt.Sprintf(" | ⏳ Baki: %d (pudhchya run madhe)", remaining)
	}
	if !completed && remaining == 0 {
		status += " | ⚠️ Run adhich thambla (connection/stop) — pudhchya run madhe continue"
	}
	if t.MirrorTopics && !mirror {
		status += "\nℹ️ Topics mirror nahi zale (source/destination forum nahi)."
	}
	if firstFail > 0 {
		status += fmt.Sprintf("\n🔁 Msg %d fail zala — pudhchya run madhe parat try hoil (3 run nantar sodun dila jato). Message harvat nahi.", firstFail)
	}
	if len(gaveUp) > 0 {
		status += fmt.Sprintf("\n⛔ 3 vela fail mhanun sodle: %v", gaveUp)
	}
	if fatalHint != "" {
		status += "\n🛠 " + fatalHint + "\n(Fix kelyavar next run ya /asrun ne tithunach continue hoil — kahi message gelele nahit.)"
	}
	if lastErr != "" {
		status += "\nLast error: " + clipRunes(lastErr, 200)
	}
	notify := manual || copied > 0 || errCount > 0 || !completed || fatalHint != ""

	return asFinish(t, status, copied, notify)
}

// ─── schedule ────────────────────────────────────────────────────────────────

var asTimeRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)

// asNormalizeTimes — "9:30, 21:00, 6pm" → ["06:00"(pm→18:00)..] sorted, unique, "HH:MM" (IST)
func asNormalizeTimes(raw string) ([]string, error) {
	fields := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		mm := asTimeRe.FindStringSubmatch(f)
		if mm == nil {
			return nil, fmt.Errorf("`%s` samajla nahi (format: 09:30 kiva 9:30pm)", f)
		}
		h, _ := strconv.Atoi(mm[1])
		min := 0
		if mm[2] != "" {
			min, _ = strconv.Atoi(mm[2])
		}
		switch mm[3] {
		case "am":
			if h < 1 || h > 12 {
				return nil, fmt.Errorf("`%s` invalid", f)
			}
			if h == 12 {
				h = 0
			}
		case "pm":
			if h < 1 || h > 12 {
				return nil, fmt.Errorf("`%s` invalid", f)
			}
			if h != 12 {
				h += 12
			}
		}
		if h > 23 || min > 59 {
			return nil, fmt.Errorf("`%s` invalid time", f)
		}
		s := fmt.Sprintf("%02d:%02d", h, min)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("kamit kami ek time dya (e.g. 09:00, 21:30)")
	}
	if len(out) > 12 {
		return nil, errors.New("max 12 times")
	}
	sort.Strings(out)
	return out, nil
}

func asParseHM(s string) (int, int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// asDue — aajcha konta hi slot (IST) nighun gela asel ani to slot var run zala nasel tar true.
func asDue(t *db.AutoSyncTask, now time.Time) bool {
	loc := istLoc()
	n := now.In(loc)
	for _, hm := range t.Times {
		h, m, ok := asParseHM(hm)
		if !ok {
			continue
		}
		slot := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, loc)
		if !slot.After(n) && t.LastRunAt.Before(slot) {
			return true
		}
	}
	return false
}

// StartAutoSync — run.go madhun goroutine mhanun start kara (primary instance var).
func StartAutoSync(ctx context.Context, _ *gotgproto.Client, log *zap.Logger) {
	asLog = log.Named("autosync")
	db.EnsureAutoSyncIndexes()
	asLog.Info("AutoSync scheduler started (checks every 30s)")

	select {
	case <-time.After(20 * time.Second): // bot + userbot boot hou dya
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		asTick(ctx)
		select {
		case <-ctx.Done():
			asLog.Info("AutoSync scheduler stopped")
			return
		case <-ticker.C:
		}
	}
}

func asTick(ctx context.Context) {
	tasks, err := db.ListAutoSyncTasks()
	if err != nil {
		asLog.Warn("AutoSync: tasks load fail", zap.Error(err))
		return
	}
	now := time.Now()
	for i := range tasks {
		t := tasks[i]
		if !t.Enabled || !asDue(&t, now) {
			continue
		}
		id := t.ID.Hex()
		if _, running := asRunning.Load(id); running {
			continue
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					asLog.Error("AutoSync task panic", zap.Any("panic", r), zap.String("task", id))
					asRunning.Delete(id)
				}
			}()
			runAutoSyncTask(ctx, id, false)
		}()
	}
}
