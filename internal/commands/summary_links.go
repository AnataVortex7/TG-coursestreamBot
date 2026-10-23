// internal/commands/summary_links.go
//
// "Summary" msg (channel chya shevti asleli topic list — topic che naav = link) handle karto.
//
//	A) /userbotdl (send channel) :
//	   - text format (bold, link vagaire entities) tasach jato (aadhi fakt plain text jat hota)
//	   - links (t.me/c/<source>/<msgID>) send channel che NAVIN msg id vaprun rewrite hotat
//	   - summary msg id DB madhe save hoto (course delivery sathi)
//
//	B) Admin ->  user la course (SendCourseRange) :
//	   - shevti summary msg user la jato; links = bot ne user la pathavlelya msgs che
//	     tg://openmessage deep links (user ne click kela ki tya msg var jato)
package commands

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// ── channel id / link helpers ────────────────────────────────────────────────

// channelRawID — -100xxxxxxxxxx / -xxxx / positive : sagle forms cha RAW (positive) ID.
func channelRawID(id int64) int64 {
	switch {
	case id < -1000000000000:
		return -(id + 1000000000000)
	case id < 0:
		return -id
	}
	return id
}

// https://t.me/c/<chan>/<msg>   kiva   https://t.me/c/<chan>/<topic>/<msg>
var tmeCLinkRe = regexp.MustCompile(`^(?:https?://)?(?:www\.)?t\.me/c/(\d+)/(?:(\d+)/)?(\d+)(?:[/?#].*)?$`)

func parseTmeCLink(raw string) (chanRaw int64, topic, msg int, ok bool) {
	m := tmeCLinkRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, 0, 0, false
	}
	chanRaw, _ = strconv.ParseInt(m[1], 10, 64)
	if m[2] != "" {
		topic, _ = strconv.Atoi(m[2])
	}
	msg, _ = strconv.Atoi(m[3])
	return chanRaw, topic, msg, msg > 0
}

// collectLinkedMsgIDs — entities madhle (chanRaw channel che) sagle unique msg IDs.
func collectLinkedMsgIDs(ents []tg.MessageEntityClass, chanRaw int64) []int {
	seen := map[int]struct{}{}
	var ids []int
	for _, e := range ents {
		tu, ok := e.(*tg.MessageEntityTextURL)
		if !ok {
			continue
		}
		ch, _, mid, ok := parseTmeCLink(tu.URL)
		if !ok || ch != chanRaw {
			continue
		}
		if _, dup := seen[mid]; !dup {
			seen[mid] = struct{}{}
			ids = append(ids, mid)
		}
	}
	return ids
}

// ── entity helpers (UTF-16 offsets) ──────────────────────────────────────────

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// entFields — entity cha Offset/Length cha pointer. Unsupported type => nil (entity drop hote).
func entFields(e tg.MessageEntityClass) (off, length *int) {
	switch v := e.(type) {
	case *tg.MessageEntityMention:
		return &v.Offset, &v.Length
	case *tg.MessageEntityHashtag:
		return &v.Offset, &v.Length
	case *tg.MessageEntityBotCommand:
		return &v.Offset, &v.Length
	case *tg.MessageEntityURL:
		return &v.Offset, &v.Length
	case *tg.MessageEntityEmail:
		return &v.Offset, &v.Length
	case *tg.MessageEntityBold:
		return &v.Offset, &v.Length
	case *tg.MessageEntityItalic:
		return &v.Offset, &v.Length
	case *tg.MessageEntityCode:
		return &v.Offset, &v.Length
	case *tg.MessageEntityPre:
		return &v.Offset, &v.Length
	case *tg.MessageEntityTextURL:
		return &v.Offset, &v.Length
	case *tg.MessageEntityMentionName:
		return &v.Offset, &v.Length
	case *tg.MessageEntityPhone:
		return &v.Offset, &v.Length
	case *tg.MessageEntityCashtag:
		return &v.Offset, &v.Length
	case *tg.MessageEntityUnderline:
		return &v.Offset, &v.Length
	case *tg.MessageEntityStrike:
		return &v.Offset, &v.Length
	case *tg.MessageEntitySpoiler:
		return &v.Offset, &v.Length
	case *tg.MessageEntityCustomEmoji:
		return &v.Offset, &v.Length
	case *tg.MessageEntityBlockquote:
		return &v.Offset, &v.Length
	}
	return nil, nil
}

// transformPiece — leading/trailing whitespace tasach thevun fakt madhlya text var fn lavto.
func transformPiece(s string, fn func(string) string) string {
	trimmedL := strings.TrimLeftFunc(s, unicode.IsSpace)
	lead := s[:len(s)-len(trimmedL)]
	core := strings.TrimRightFunc(trimmedL, unicode.IsSpace)
	trail := trimmedL[len(core):]
	if core == "" {
		return s
	}
	return lead + fn(core) + trail
}

// transformTextEntities — text var textFn (replace rules) lavto ani entities che offsets/length
// navin text pramane parat calculate karto. Text entity-boundaries nusar tukdyat todun (piece-wise)
// replace hoto — mhanje link/bold che boundary kadhi hallat nahit.
//
// linkFn(url) -> (newURL, keep): TextURL entities sathi; keep=false => entity kadhun takto
// (text rahato, link jato).
//
// NOTE: entities IN-PLACE modify hotat (caller ne navinach fetch kelela msg dyava).
func transformTextEntities(
	text string,
	ents []tg.MessageEntityClass,
	textFn func(string) string,
	linkFn func(url string) (string, bool),
) (string, []tg.MessageEntityClass) {
	u16 := utf16.Encode([]rune(text))
	n := len(u16)

	type span struct {
		e       tg.MessageEntityClass
		off, en int
	}
	cutSet := map[int]struct{}{0: {}, n: {}}
	spans := make([]span, 0, len(ents))
	for _, e := range ents {
		o, l := entFields(e)
		if o == nil || l == nil {
			continue
		}
		s, en := *o, *o+*l
		if s < 0 || s >= n {
			continue
		}
		if en > n {
			en = n
		}
		if en <= s {
			continue
		}
		spans = append(spans, span{e, s, en})
		cutSet[s] = struct{}{}
		cutSet[en] = struct{}{}
	}
	cuts := make([]int, 0, len(cutSet))
	for c := range cutSet {
		cuts = append(cuts, c)
	}
	sort.Ints(cuts)

	newPos := make(map[int]int, len(cuts))
	var sb strings.Builder
	pos := 0
	for i, c := range cuts {
		newPos[c] = pos
		if i+1 >= len(cuts) {
			break
		}
		piece := string(utf16.Decode(u16[c:cuts[i+1]]))
		if textFn != nil {
			piece = transformPiece(piece, textFn)
		}
		sb.WriteString(piece)
		pos += utf16Len(piece)
	}

	out := make([]tg.MessageEntityClass, 0, len(spans))
	for _, sp := range spans {
		no, ne := newPos[sp.off], newPos[sp.en]
		if ne <= no {
			continue
		}
		o, l := entFields(sp.e)
		*o, *l = no, ne-no
		if tu, ok := sp.e.(*tg.MessageEntityTextURL); ok && linkFn != nil {
			nu, keep := linkFn(tu.URL)
			if !keep {
				continue
			}
			tu.URL = nu
		}
		out = append(out, sp.e)
	}
	return sb.String(), out
}

// ── A) send channel sathi text prepare ───────────────────────────────────────

// prepareTextForSendChannel — text + entities (links = send channel che navin msg id) tayar karto.
// returns: text, entities, linkCount (source channel kade janare links), unresolved (jyanche target
// ajun copy zalele nahit — nantar resync karto).
func prepareTextForSendChannel(
	tgMsg *tg.Message, adminChatID, srcRaw, destRaw int64, topicID int,
) (string, []tg.MessageEntityClass, int, int) {
	ids := collectLinkedMsgIDs(tgMsg.Entities, srcRaw)
	idMap, _ := db.GetUserbotMsgMap(srcRaw, destRaw, ids)
	unresolved := 0
	rewrite := func(u string) (string, bool) {
		ch, _, mid, ok := parseTmeCLink(u)
		if !ok || ch != srcRaw {
			return u, true // baher cha / vegla channel cha link — jasa aahe tasa
		}
		nid, found := idMap[mid]
		if !found {
			unresolved++
			return u, true // target ajun copy zala nahi — resync nantar fix karto
		}
		if topicID > 0 {
			return fmt.Sprintf("https://t.me/c/%d/%d/%d", destRaw, topicID, nid), true
		}
		return fmt.Sprintf("https://t.me/c/%d/%d", destRaw, nid), true
	}
	text, ents := transformTextEntities(tgMsg.Message, tgMsg.Entities,
		func(s string) string { return applyReplacesForUser(adminChatID, s) }, rewrite)
	return text, ents, len(ids), unresolved
}

// sendTextOnlyCopy — Phase B madhe (media nasleli) text msg copy. Phase B sequential asto, mhanun
// aadhiche sagle lectures chi msgID mapping DB madhe aadhich save zalele astat.
func sendTextOnlyCopy(
	botClient *gotgproto.Client,
	chatIDRaw int64,
	msgID, topicID int,
	botPeer tg.InputPeerClass,
	sourcePeer, sendChannelPeer *tg.InputPeerChannel,
	pm *preparedMessage,
) error {
	finalCaption := pm.finalCaption

	if sendChannelPeer != nil {
		text, ents, linkCount, unresolved := prepareTextForSendChannel(
			pm.tgMsg, pm.adminChatID, chatIDRaw, sendChannelPeer.ChannelID, topicID)
		if strings.TrimSpace(text) == "" {
			text, ents = finalCaption, nil
		}
		if strings.TrimSpace(text) == "" {
			return nil // rikama msg — kahi pathavaycha nahi
		}

		scCtx, scCancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer scCancel()
		req := &tg.MessagesSendMessageRequest{
			Peer:      sendChannelPeer,
			Message:   text,
			Entities:  ents,
			RandomID:  rand.Int63(),
			NoWebpage: linkCount > 0,
		}
		if topicID > 0 {
			req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: topicID}
		}
		res, err := botClient.API().MessagesSendMessage(scCtx, req)
		if err != nil {
			return fmt.Errorf("send channel text send failed: %w", err)
		}
		if newID := extractMessageID(res); newID > 0 {
			if e := db.SaveUserbotMsgMap(chatIDRaw, msgID, sendChannelPeer.ChannelID, newID); e != nil {
				utils.Logger.Warn("msgmap save failed", zap.Error(e))
			}
			if linkCount > 0 {
				if e := db.SaveUserbotSummary(chatIDRaw, msgID, sendChannelPeer.ChannelID, newID, unresolved); e != nil {
					utils.Logger.Warn("summary save failed", zap.Error(e))
				}
			}
		}

		go func() {
			lCtx, lCancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer lCancel()
			logCh, lerr := utils.GetLogChannelPeer(lCtx, botClient.API(), botClient.PeerStorage)
			if lerr != nil {
				return
			}
			_, _ = botClient.API().MessagesSendMessage(lCtx, &tg.MessagesSendMessageRequest{
				Peer:      &tg.InputPeerChannel{ChannelID: logCh.ChannelID, AccessHash: logCh.AccessHash},
				Message:   text,
				Entities:  ents,
				RandomID:  rand.Int63(),
				Silent:    true,
				NoWebpage: linkCount > 0,
			})
		}()
		return nil
	}

	// ── send channel nasel tar : junach behavior (admin chat + source channel) ──
	textContent := fmt.Sprintf(
		"📥 Userbot DL\n📌 Chat: `%d` | Msg: `%d`\n\n%s",
		chatIDRaw, msgID, finalCaption,
	)
	sendCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	if _, err := botClient.API().MessagesSendMessage(sendCtx, &tg.MessagesSendMessageRequest{
		Peer:     botPeer,
		Message:  textContent,
		RandomID: rand.Int63(),
	}); err != nil {
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
	return nil
}

// resyncUnresolvedSummaries — retry pass nantar : jya summary msgs madhle links cha target tevha copy
// navhta zala, tyanna send channel madhe EDIT karun links fix karto.
func resyncUnresolvedSummaries(
	ctx context.Context,
	botClient, userbotClient *gotgproto.Client,
	chatID, chatIDRaw int64,
	sendChannelPeer *tg.InputPeerChannel,
	topicID int,
	adminChatID int64,
) (fixed int) {
	if sendChannelPeer == nil || userbotClient == nil {
		return 0
	}
	list, err := db.ListUnresolvedSummaries(chatIDRaw, sendChannelPeer.ChannelID)
	if err != nil {
		return 0
	}
	for _, s := range list {
		m, err := utils.GetTGMessageInChannel(ctx, userbotClient, s.SrcMsgID, chatID)
		if err != nil {
			continue
		}
		text, ents, _, unresolved := prepareTextForSendChannel(m, adminChatID, chatIDRaw, sendChannelPeer.ChannelID, topicID)
		if strings.TrimSpace(text) == "" {
			continue
		}
		eCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, eErr := botClient.API().MessagesEditMessage(eCtx, &tg.MessagesEditMessageRequest{
			Peer:      sendChannelPeer,
			ID:        s.DestMsgID,
			Message:   text,
			Entities:  ents,
			NoWebpage: true,
		})
		cancel()
		if eErr != nil && !strings.Contains(strings.ToUpper(eErr.Error()), "MESSAGE_NOT_MODIFIED") {
			utils.Logger.Warn("summary resync edit failed", zap.Error(eErr), zap.Int("destMsg", s.DestMsgID))
			continue
		}
		_ = db.SaveUserbotSummary(chatIDRaw, s.SrcMsgID, sendChannelPeer.ChannelID, s.DestMsgID, unresolved)
		fixed++
	}
	return fixed
}

// ── B) course user la dyaychya veli summary ──────────────────────────────────

// sendCourseSummaries — course channel (send channel) madhle summary msgs user la pathavto.
// Links = bot ne user chya chat madhe pathavlelya lecture msgs che tg://openmessage deep links.
// Jya lecture user la pathavla nahi tyacha link kadhun taklela (channel link leak hot nahi).
// returns: kiti summary msgs pathavle.
func sendCourseSummaries(botClient *gotgproto.Client, user *db.User, course *db.Course, courseID primitive.ObjectID) int {
	if botClient == nil || botClient.Self == nil || user == nil || course == nil {
		return 0
	}
	destRaw := channelRawID(course.ChannelID)
	sums, err := db.ListUserbotSummaries(destRaw)
	if err != nil || len(sums) == 0 {
		return 0
	}
	chatID := user.TelegramID
	botID := botClient.Self.ID
	sentMap, _ := db.GetLectureMsgMap(user.ID, courseID, chatID, botID)
	if len(sentMap) == 0 {
		return 0
	}

	bg := context.Background()
	gctx := botClient.CreateContext()
	peer := gctx.PeerStorage.GetInputPeerById(chatID)
	if peer.Zero() {
		return 0
	}
	chPeer, err := utils.GetChannelPeer(bg, botClient.API(), botClient.PeerStorage, course.ChannelID)
	if err != nil || chPeer == nil {
		return 0
	}

	count := 0
	for _, s := range sums {
		res, gErr := botClient.API().ChannelsGetMessages(bg, &tg.ChannelsGetMessagesRequest{
			Channel: chPeer,
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: s.DestMsgID}},
		})
		if gErr != nil {
			continue
		}
		msgs, ok := res.(*tg.MessagesChannelMessages)
		if !ok || len(msgs.Messages) == 0 {
			continue
		}
		m, ok := msgs.Messages[0].(*tg.Message)
		if !ok || strings.TrimSpace(m.Message) == "" {
			continue
		}

		resolved := 0
		rewrite := func(u string) (string, bool) {
			ch, _, mid, ok := parseTmeCLink(u)
			if !ok || ch != destRaw {
				return u, true // baher cha link — jasa aahe tasa
			}
			sid, found := sentMap[mid]
			if !found {
				return "", false // ya user la ha lecture pathavla nahi — link kadhun taka
			}
			resolved++
			return fmt.Sprintf("tg://openmessage?user_id=%d&message_id=%d", botID, sid), true
		}
		text, ents := transformTextEntities(m.Message, m.Entities, nil, rewrite)
		if resolved == 0 {
			continue // ha summary ya course/range cha nahi
		}

		sendRes, sErr := botClient.API().MessagesSendMessage(bg, &tg.MessagesSendMessageRequest{
			Peer:      peer,
			Message:   text,
			Entities:  ents,
			RandomID:  rand.Int63(),
			NoWebpage: true,
		})
		if sErr != nil {
			utils.Logger.Warn("course summary send failed", zap.Error(sErr))
			continue
		}
		count++
		if newID := extractMessageID(sendRes); newID > 0 {
			_ = db.RecordSentLectureMsg(user.ID, courseID, chatID, newID, botID)
			_ = db.AddPendingDeletion(user.ID, courseID, chatID, newID, botID)
		}
		time.Sleep(1 * time.Second)
	}
	return count
}
