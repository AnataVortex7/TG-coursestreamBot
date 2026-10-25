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
	"github.com/gotd/td/bin"
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

// ─── connection resilience ───────────────────────────────────────────────────
// Karan: pahile run madhe client ek vela ghetla jaycha ani connection tutla ("engine forcibly
// closed: context canceled") ki run lagech thambaycha + last_run_at set vhaycha => pudhcha run
// dusrya divshi. Aata: transient error aala ki client refresh (revive) karun SAME message parat
// pathavto (deterministic RandomID mule duplicate hot nahi).

const (
	asMaxConnRetries = 8 // ek message/step sathi connection-error retries
	asMaxConnRuns    = 6 // sallagpane itkya runs connection mule adhurya rahile tar shevti stamp
)

// asDstClient — DESTINATION cha sarva kaam (send / upload / topic / edit) course BOT kadun hota.
// Userbot cha kaam fakt SOURCE madhun history fetch ani file download. (Bot destination madhe owner/admin ahe.)
func asDstClient() (*gotgproto.Client, error) {
	if MainBotClient == nil {
		return nil, errors.New("main bot client ready nahi")
	}
	return MainBotClient, nil
}

// asBotAPI — bot cha tg.Client (nil-safe nahi; run chya survatila asDstClient() ne check hoto).
func asBotAPI() *tg.Client { return MainBotClient.API() }

// asClientHolder — run chalu astana client badalta yava mhanun.
type asClientHolder struct {
	mu  sync.Mutex
	c   *gotgproto.Client
	key string
}

func (h *asClientHolder) Get() *gotgproto.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

// Refresh — client jivant nasel tar navin ghet.
func (h *asClientHolder) Refresh(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.c != nil && pingUserbot(ctx, h.c) {
		return nil
	}
	c, err := asClientForKey(ctx, h.key)
	if err != nil {
		return err
	}
	h.c = c
	return nil
}

// asBackoff — connection error nantar thamba (ctx cancel zala tar lagech sutto).
func asBackoff(ctx context.Context, n int) {
	d := time.Duration(n) * 5 * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// asRetryConn — fn la connection error aala tar client refresh karun parat try karto.
func asRetryConn[T any](ctx context.Context, h *asClientHolder, fn func(c *gotgproto.Client) (T, error)) (T, error) {
	var zero T
	for n := 0; ; n++ {
		v, err := fn(h.Get())
		if err == nil || !isTransientConnErr(err) || ctx.Err() != nil || n >= asMaxConnRetries {
			return v, err
		}
		asBackoff(ctx, n+1)
		_ = h.Refresh(ctx)
		if ctx.Err() != nil {
			return zero, err
		}
	}
}

type asConnState struct {
	n    int
	next time.Time
}

var asConnRetry sync.Map // task hex ID -> *asConnState

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
func asEnsureTopic(ctx context.Context, srcAPI, api *tg.Client, t *db.AutoSyncTask, src, dst *asChat, srcTopic int) (int, error) {
	if srcTopic <= 1 {
		return 0, nil // General topic
	}
	if d, ok := db.AutoSyncTopicGet(t.ID, srcTopic); ok {
		return d, nil
	}
	title := fmt.Sprintf("Topic %d", srcTopic)
	if res, err := srcAPI.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
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

// asApplyRules — SARVA replace rules ekatra:
//  1) global (admin) rules — /userbotdl sarkhech, sarvansathi common
//  2) task cha owner cha personal rules (non-admin asel tar)
//  3) userbot account cha owner (session user_<id>) cha personal rules
//  4) task-specific rules (/asreplace)
func asApplyRules(t *db.AutoSyncTask, s string) string {
	s = applyReplacesForUser(t.OwnerID, s)
	if strings.HasPrefix(t.SessionKey, "user_") {
		if uid, err := strconv.ParseInt(strings.TrimPrefix(t.SessionKey, "user_"), 10, 64); err == nil &&
			uid != t.OwnerID && !isReplaceAdmin(uid) {
			s = applyRules(s, db.GetReplaceRules(uid))
		}
	}
	return applyRules(s, t.Replaces)
}

// asCloneEntities — entities deep copy (transformTextEntities IN-PLACE modify karto; retry var double-shift nako).
func asCloneEntities(in []tg.MessageEntityClass) []tg.MessageEntityClass {
	out := make([]tg.MessageEntityClass, 0, len(in))
	for _, e := range in {
		var b bin.Buffer
		if err := e.Encode(&b); err != nil {
			continue
		}
		c, err := tg.DecodeMessageEntity(&b)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// asTransformText — message text/caption + entities var sarva replace rules lavto.
// Formatting (bold/link/spoiler...) tikavto: entity-aware (offsets navin text pramane). Link URLs var pan rules lagtat.
// Jar rule ek formatting boundary olandto (piece-wise match hot nahi), tar plain full-text replace (entities nahit).
func asTransformText(t *db.AutoSyncTask, m *tg.Message) (string, []tg.MessageEntityClass) {
	full := asApplyRules(t, m.Message)
	ents := asCloneEntities(m.Entities)
	pieceText, pieceEnts := transformTextEntities(m.Message, ents,
		func(x string) string { return asApplyRules(t, x) },
		func(u string) (string, bool) {
			nu := asApplyRules(t, u)
			if strings.TrimSpace(nu) == "" {
				return u, true
			}
			return nu, true
		})
	if pieceText == full {
		return pieceText, pieceEnts
	}
	if full == m.Message {
		return m.Message, m.Entities
	}
	return full, nil
}

// asSendOpt — Placeholder: asli media na pathvta same-type chhota fake file pathva.
// EditID > 0: navin message na pathvta tya destination message chi media asli media ne replace kar.
type asSendOpt struct {
	Placeholder bool
	EditID      int
}

func asSendOne(ctx context.Context, c *gotgproto.Client, t *db.AutoSyncTask, dst *asChat, m *tg.Message, destTopic, replyDest int, opt asSendOpt) (int, error) {
	// c = userbot (SOURCE: fakt download). Destination la sarva kaam BOT (dstC) kadun.
	dstC, derr := asDstClient()
	if derr != nil {
		return 0, derr
	}
	srcAPI := c.API()
	api := dstC.API()
	tid := t.ID.Hex()

	newText, entities := asTransformText(t, m)

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
		// Media type copy karta yet nahi (paid/story/giveaway/unsupported...) — caption text jato, media nahi.
		// Gap lapu naye mhanun log + progress madhe type dakhavto.
		if asLog != nil {
			asLog.Warn(fmt.Sprintf("AutoSync msg %d: media type %T copy hot nahi — fakt caption text pathvla", m.ID, media))
		}
		asNoteMediaSkipped(tid, m.ID, fmt.Sprintf("%T", media))
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
			if _, err := downloader.NewDownloader().Download(srcAPI, f.Location).Stream(ctx, &buf); err != nil {
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
			upFile, tmpPath, err := downloadAndReupload(ctx, ctx, c, dstC, f, onDl, onUp)
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
		if opt.EditID > 0 { // placeholder → asli media replace
			_, err := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
				return api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
					Peer:     dst.Peer,
					ID:       opt.EditID,
					Media:    im,
					Message:  caption,
					Entities: capEnts,
				})
			})
			if err != nil && !strings.Contains(strings.ToUpper(err.Error()), "MESSAGE_NOT_MODIFIED") {
				return 0, err
			}
			return opt.EditID, nil
		}
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

	// Placeholder: same type (jpg→jpg, mp4→mp4, mkv→mkv) chhota fake file + caption.
	buildPlaceholder := func() (tg.InputMediaClass, error) {
		switch md := media.(type) {
		case *tg.MessageMediaPhoto:
			up, err := uploader.NewUploader(api).FromBytes(ctx, "photo.jpg", asPlaceholderBytes("jpg"))
			if err != nil {
				return nil, err
			}
			return &tg.InputMediaUploadedPhoto{File: up}, nil
		case *tg.MessageMediaDocument:
			d, ok := md.Document.AsNotEmpty()
			if !ok {
				return nil, errors.New("document empty")
			}
			name := "file"
			for _, a := range d.Attributes {
				if fa, ok := a.(*tg.DocumentAttributeFilename); ok && fa.FileName != "" {
					name = fa.FileName
				}
			}
			_, data := asPlaceholderFor(name, d.MimeType)
			up, err := uploader.NewUploader(api).FromBytes(ctx, name, data)
			if err != nil {
				return nil, err
			}
			return &tg.InputMediaUploadedDocument{File: up, MimeType: d.MimeType, Attributes: d.Attributes}, nil
		}
		return nil, errAsUnsupported
	}
	if opt.Placeholder {
		im, err := buildPlaceholder()
		if err != nil {
			return 0, err
		}
		destID, err := sendMedia(im)
		if err != nil {
			return 0, err
		}
		if followText != "" {
			_, _ = sendText(followText, followEnts, "cap")
		}
		return destID, nil
	}

	var destID int
	var sendErr error
	tryFull := true // fast (file-reference copy) userbot->bot madhe chalat nahi — nehmi full (userbot download → bot upload)

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

	if followText != "" && opt.EditID == 0 {
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
		{"CHAT_ADMIN_REQUIRED", "Bot la destination madhe ADMIN right dya (Post Messages + Manage Topics + Edit Messages)."},
		{"CHAT_WRITE_FORBIDDEN", "Bot la destination madhe post karnyachi parvangi nahi."},
		{"CHAT_SEND_PLAIN_FORBIDDEN", "Destination madhe text pathvnyachi parvangi nahi."},
		{"CHAT_SEND_MEDIA_FORBIDDEN", "Destination madhe media pathvnyachi parvangi nahi."},
		{"CHAT_SEND_PHOTOS_FORBIDDEN", "Destination madhe photos pathvnyachi parvangi nahi."},
		{"CHAT_SEND_VIDEOS_FORBIDDEN", "Destination madhe videos pathvnyachi parvangi nahi."},
		{"CHAT_SEND_DOCS_FORBIDDEN", "Destination madhe files pathvnyachi parvangi nahi."},
		{"USER_BANNED_IN_CHANNEL", "Bot destination madhun ban ahe."},
		{"CHANNEL_PRIVATE", "Userbot (source) kiva Bot (destination) la chat cha access nahi (left / banned)."},
		{"CHANNEL_INVALID", "Chat access invalid — source madhe userbot ani destination madhe bot ahe ka?"},
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

// asIsFileRefErr — Telegram file reference expire/invalid (message jun/old fetch zala).
func asIsFileRefErr(err error) bool {
	if err == nil {
		return false
	}
	up := strings.ToUpper(err.Error())
	return strings.Contains(up, "FILE_REFERENCE_EXPIRED") ||
		strings.Contains(up, "FILE_REFERENCE_EMPTY") ||
		strings.Contains(up, "FILE_REFERENCE_INVALID") ||
		strings.Contains(up, "FILE_REFERENCE_")
}

// asRefetchMsg — source message parat Telegram kadun ghet (taaje file reference milavnyasathi).
func asRefetchMsg(ctx context.Context, h *asClientHolder, src *asChat, id int) (*tg.Message, error) {
	res, err := asRetryConn(ctx, h, func(c *gotgproto.Client) (tg.MessagesMessagesClass, error) {
		return utils.ExecuteWithFloodWait(ctx, func() (tg.MessagesMessagesClass, error) {
			return c.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: src.Input,
				ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: id}},
			})
		})
	})
	if err != nil {
		return nil, err
	}
	var list []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesChannelMessages:
		list = r.Messages
	case *tg.MessagesMessagesSlice:
		list = r.Messages
	case *tg.MessagesMessages:
		list = r.Messages
	}
	for _, mc := range list {
		if mm, ok := mc.(*tg.Message); ok && mm.ID == id {
			return mm, nil
		}
	}
	return nil, errors.New("message source madhe sapadla nahi (delete zala?)")
}

var asMediaSkipNote sync.Map // task hex ID -> string (shevtcha unsupported media message/type)

func asNoteMediaSkipped(tid string, msgID int, typ string) {
	asMediaSkipNote.Store(tid, fmt.Sprintf("msg %d (%s)", msgID, typ))
}

// asReplacePlaceholders — junya run madhe fail zalelya media sathi jo fake file takla hota, tyala
// asli media ne EDIT karun replace karto (message jagevarach rahto, order bighadat nahi).
func asReplacePlaceholders(ctx context.Context, h *asClientHolder, t *db.AutoSyncTask, src, dst *asChat, taskID string) (done, failed int, left int) {
	list := db.AutoSyncPlaceholderList(t.ID, 6, 100)
	if len(list) == 0 {
		return 0, 0, db.AutoSyncPlaceholderCount(t.ID)
	}
	asProgUpdate(taskID, func(p *asProgress) {
		p.Phase = fmt.Sprintf("🧩 Placeholder → asli media replace (%d)", len(list))
	})
	for _, ph := range list {
		if ctx.Err() != nil {
			break
		}
		m, err := asRefetchMsg(ctx, h, src, ph.SrcMsg)
		if err != nil {
			if strings.Contains(err.Error(), "sapadla nahi") { // source delete zala — placeholder tasach thev
				db.AutoSyncPlaceholderDone(t.ID, ph.SrcMsg)
				continue
			}
			db.AutoSyncPlaceholderTry(t.ID, ph.SrcMsg)
			failed++
			continue
		}
		var serr error
		for n := 0; n <= 3; n++ {
			_, serr = asSendOne(ctx, h.Get(), t, dst, m, 0, 0, asSendOpt{EditID: ph.DestMsg})
			if serr == nil || ctx.Err() != nil {
				break
			}
			if isTransientConnErr(serr) {
				asBackoff(ctx, n+1)
				_ = h.Refresh(ctx)
				continue
			}
			if asIsFileRefErr(serr) {
				if fresh, ferr := asRefetchMsg(ctx, h, src, ph.SrcMsg); ferr == nil {
					m = fresh
				}
				continue
			}
			break
		}
		if serr == nil {
			db.AutoSyncPlaceholderDone(t.ID, ph.SrcMsg)
			done++
		} else if ctx.Err() == nil {
			db.AutoSyncPlaceholderTry(t.ID, ph.SrcMsg)
			failed++
			if asLog != nil {
				asLog.Warn(fmt.Sprintf("AutoSync placeholder replace fail msg %d: %v", ph.SrcMsg, serr))
			}
		}
		select {
		case <-time.After(asPauseFor(m)):
		case <-ctx.Done():
		}
	}
	return done, failed, db.AutoSyncPlaceholderCount(t.ID)
}

// asEnvMs — env var (milliseconds) vachto, nasel/chukicha tar def.
func asEnvMs(name string, def int) time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(def) * time.Millisecond
}

// asPauseFor — message nantar kiti thambaycha (text-only fast, media thoda jast).
func asPauseFor(m *tg.Message) time.Duration {
	switch m.Media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return asEnvMs("AUTOSYNC_TEXT_DELAY_MS", 500)
	}
	return asEnvMs("AUTOSYNC_MEDIA_DELAY_MS", 1200)
}

func asFinish(t *db.AutoSyncTask, status string, copied int, notify bool) string {
	return asFinishEx(t, status, copied, notify, true)
}

// asFinishEx — stamp=false: last_run_at set karat nahi, mhanje scheduler lavkarch parat chalvel.
func asFinishEx(t *db.AutoSyncTask, status string, copied int, notify, stamp bool) string {
	asProgFinish(t.ID.Hex())
	f := bson.M{"last_status": status}
	if stamp {
		f["last_run_at"] = time.Now()
	}
	_ = db.SetAutoSyncFields(t.ID, f)
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

	// FloodWait madhe thambla tar progress madhe disel (elapsed vadhto pan index nahi — tyacha karan).
	ctx = utils.WithFloodWaitNotify(ctx, func(sec int) {
		asProgUpdate(taskID, func(p *asProgress) {
			p.Phase = fmt.Sprintf("⏳ Telegram FloodWait — %d sec thambla ahe (aapoaap continue hoil)", sec)
		})
	})

	client, err := asClientForKey(ctx, t.SessionKey)
	if err != nil {
		return asFinishEx(t, "❌ Userbot problem: "+err.Error(), 0, true, false)
	}
	holder := &asClientHolder{c: client, key: t.SessionKey}
	src, err := asRetryConn(ctx, holder, func(c *gotgproto.Client) (*asChat, error) {
		return asResolveChat(ctx, c, t.SourceChat)
	})
	if err != nil {
		return asFinishEx(t, "❌ Source: "+err.Error(), 0, true, !isTransientConnErr(err))
	}
	dstC, derr := asDstClient()
	if derr != nil {
		return asFinishEx(t, "❌ Destination: "+derr.Error(), 0, true, false)
	}
	dst, err := asResolveChat(ctx, dstC, t.DestChat) // DESTINATION = BOT (userbot nahi)
	if err != nil {
		return asFinishEx(t, "❌ Destination: bot la chat "+strconv.FormatInt(t.DestChat, 10)+" sapadla nahi — bot tya channel madhe owner/admin ahe ka? ("+err.Error()+")", 0, true, !isTransientConnErr(err))
	}
	if !dst.CanPost {
		return asFinishEx(t, "❌ Destination: bot la tya channel madhe post karnyacha (admin) access nahi.", 0, true, true)
	}

	type fetchRes struct {
		msgs    []*tg.Message
		maxSeen int
	}
	fr, err := asRetryConn(ctx, holder, func(c *gotgproto.Client) (fetchRes, error) {
		m, mx, e := asFetchNew(ctx, c.API(), src.Peer, t.LastMsgID)
		return fetchRes{m, mx}, e
	})
	if err != nil {
		return asFinishEx(t, "❌ Source history fetch fail: "+err.Error(), 0, true, !isTransientConnErr(err))
	}
	msgs, maxSeen := fr.msgs, fr.maxSeen
	fetchedAt := time.Now() // file reference fakt kahi vel valid asto — jun zala tar refetch karto

	// Junya run che placeholders (fake media) aadhi asli media ne replace kara.
	phDone, phFailed, _ := asReplacePlaceholders(ctx, holder, t, src, dst, taskID)
	if phDone+phFailed > 0 {
		fetchedAt = time.Now()
	}
	phNew := 0 // ya run madhe nave placeholders

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
	connAbort := false // connection mule run adhura rahila (retries sampalya)
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
			p.Phase = "📨 Messages pathvat ahe"
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
				asProgUpdate(taskID, func(p *asProgress) { p.Phase = "📂 Destination topic check/create hot ahe" })
				dt, terr := asRetryConn(ctx, holder, func(c *gotgproto.Client) (int, error) {
					return asEnsureTopic(ctx, c.API(), asBotAPI(), t, src, dst, srcTopic)
				})
				if terr != nil {
					if isTransientConnErr(terr) {
						connAbort = true
					}
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
		refErrs := 0
		hasMedia := false
		switch m.Media.(type) {
		case *tg.MessageMediaPhoto, *tg.MessageMediaDocument:
			hasMedia = true
		}
		if hasMedia && time.Since(fetchedAt) > 3*time.Minute {
			if fresh, ferr := asRefetchMsg(ctx, holder, src, m.ID); ferr == nil {
				m = fresh
			}
		}
		connFails := 0
		for attempt := 1; attempt <= 3; attempt++ {
			destID, sendErr = asSendOne(ctx, holder.Get(), t, dst, m, destTopic, replyDest, asSendOpt{})
			if sendErr == nil || ctx.Err() != nil || errors.Is(sendErr, errAsUnsupported) {
				break
			}
			if isTransientConnErr(sendErr) {
				// Run thambvu nako: client refresh karun SAME message parat pathva.
				connFails++
				if connFails > asMaxConnRetries {
					aborted = true
					connAbort = true
					break
				}
				asProgUpdate(taskID, func(p *asProgress) {
					p.Phase = fmt.Sprintf("🔄 Connection tutla — reconnect (%d/%d)", connFails, asMaxConnRetries)
				})
				asBackoff(ctx, connFails)
				_ = holder.Refresh(ctx)
				attempt-- // connection retry mhanje normal attempt khat nahi
				continue
			}
			if hasMedia && asIsFileRefErr(sendErr) && refErrs < 3 {
				// FILE_REFERENCE_EXPIRED: message parat fetch kar (taaje reference) ani SAME message retry.
				refErrs++
				if fresh, ferr := asRefetchMsg(ctx, holder, src, m.ID); ferr == nil {
					m = fresh
				}
				asBackoff(ctx, 1)
				attempt--
				continue
			}
			if fatal, hint := asIsFatalErr(sendErr); fatal {
				fatalHint = hint
				aborted = true
				break
			}
			// destination topic delete zala asel — mapping kadhun topic parat banva, mag retry
			if mirror && destTopic > 0 && asIsTopicGoneErr(sendErr) {
				db.AutoSyncTopicDelete(t.ID, srcTopic)
				if dt, terr := asEnsureTopic(ctx, holder.Get().API(), asBotAPI(), t, src, dst, srcTopic); terr == nil {
					destTopic = dt
					continue
				}
			}
			select {
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			case <-ctx.Done():
			}
		}
		// Media copy fail zali (retries nantar) — message jagevar rahava mhanun same-type fake media + caption pathva.
		if sendErr != nil && !aborted && ctx.Err() == nil && hasMedia && !errors.Is(sendErr, errAsUnsupported) {
			origErr := sendErr
			pdID, perr := asRetryConn(ctx, holder, func(c *gotgproto.Client) (int, error) {
				return asSendOne(ctx, c, t, dst, m, destTopic, replyDest, asSendOpt{Placeholder: true})
			})
			if perr == nil {
				destID, sendErr = pdID, nil
				phNew++
				_ = db.AutoSyncMapPutPlaceholder(t.ID, m.ID, pdID)
				if asLog != nil {
					asLog.Warn(fmt.Sprintf("AutoSync msg %d: media copy fail (%v) — placeholder pathvla, next run madhe replace hoil", m.ID, origErr))
				}
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

		// Flood avoid: pratyek message nantar thoda thamba.
		// Text-only = AUTOSYNC_TEXT_DELAY_MS (default 500ms), media = AUTOSYNC_MEDIA_DELAY_MS (default 1200ms).
		// FloodWait aala tar ExecuteWithFloodWait aapoaap thambto ani parat try karto.
		pause := asPauseFor(m)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
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
	if phNew > 0 {
		status += fmt.Sprintf("\n🧩 %d media copy zali nahi — same-type placeholder pathvla (jagevar rahila). Pudhchya run madhe asli media replace hoil.", phNew)
	}
	if phDone > 0 {
		status += fmt.Sprintf("\n✅ %d placeholder → asli media replace zali.", phDone)
	}
	if phFailed > 0 {
		status += fmt.Sprintf("\n⚠️ %d placeholder replace fail (parat try hoil).", phFailed)
	}
	if v, ok := asMediaSkipNote.LoadAndDelete(taskID); ok {
		status += fmt.Sprintf("\n⚠️ Media copy hot nahi (fakt caption gela): %v", v)
	}
	// Connection mule adhura run: last_run_at stamp karu nako => scheduler (30s tick) auto-resume karel.
	stamp := true
	if connAbort {
		v, _ := asConnRetry.LoadOrStore(taskID, &asConnState{})
		st := v.(*asConnState)
		st.n++
		if st.n < asMaxConnRuns {
			wait := time.Duration(st.n) * 2 * time.Minute
			st.next = time.Now().Add(wait)
			stamp = false
			status += fmt.Sprintf("\n🔄 Connection problem — %d min nantar aapoaap parat chalel (%d/%d).", int(wait.Minutes()), st.n, asMaxConnRuns)
		} else {
			asConnRetry.Delete(taskID)
			status += "\n⛔ Sallagpane connection fail — pudhchya scheduled time la parat try hoil (kiva /asrun)."
		}
	} else {
		asConnRetry.Delete(taskID)
	}
	notify := manual || copied > 0 || errCount > 0 || !completed || fatalHint != ""

	return asFinishEx(t, status, copied, notify, stamp)
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
		if v, ok := asConnRetry.Load(id); ok && time.Now().Before(v.(*asConnState).next) {
			continue // connection backoff chalu ahe
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
