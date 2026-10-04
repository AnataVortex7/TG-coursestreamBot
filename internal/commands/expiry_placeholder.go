// internal/commands/expiry_placeholder.go
//
// Course expire झाल्यावर student च्या chat मधले lecture messages delete करायचा
// प्रयत्न होतो. Telegram bot ला ~48 तासांपेक्षा जुने messages delete करू देत नाही
// (MESSAGE_DELETE_FORBIDDEN, किंवा काहीच delete न होता silently 0 affected).
// पण bot स्वतःचे जुने messages EDIT करू शकतो — म्हणून delete जमलं नाही तर:
//   - video  → एकच same dummy video (LOG_CHANNEL मधला एक message)
//   - PDF/file → एकच same dummy PDF
//   - photo  → एकच same dummy photo
//     आणि caption "course expire — renew करा" + 🔁 Renew बटण.
//
// Dummy files प्रत्येक वेळी re-upload होत नाहीत: पहिल्यांदा LOG_CHANNEL मध्ये एकदा
// पाठवतो, त्याचा message ID app_settings मध्ये save, पुढे तोच reuse (file reference
// ताजा घेऊन). Admin ने LOG_CHANNEL मधला तो message बदलला/delete केला तर आपोआप परत तयार होतो.
package commands

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

const (
	phVideo = "video"
	phPDF   = "pdf"
	phPhoto = "photo"
)

// Tiny generated assets (video ~2KB, pdf ~0.6KB, photo ~2.7KB). "COURSE EXPIRED —
// Renew to watch again" असं लिहिलेलं. LOG_CHANNEL मध्ये एकदा upload होतात.
const (
	b64PlaceholderVideo = "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"
	b64PlaceholderPDF   = "JVBERi0xLjQKMSAwIG9iago8PCAvVHlwZSAvQ2F0YWxvZyAvUGFnZXMgMiAwIFIgPj4KZW5kb2JqCjIgMCBvYmoKPDwgL1R5cGUgL1BhZ2VzIC9LaWRzIFszIDAgUl0gL0NvdW50IDEgPj4KZW5kb2JqCjMgMCBvYmoKPDwgL1R5cGUgL1BhZ2UgL1BhcmVudCAyIDAgUiAvTWVkaWFCb3ggWzAgMCAzMDAgMTUwXSAvQ29udGVudHMgNCAwIFIgL1Jlc291cmNlcyA8PCAvRm9udCA8PCAvRjEgNSAwIFIgPj4gPj4gPj4KZW5kb2JqCjQgMCBvYmoKPDwgL0xlbmd0aCA1OSA+PgpzdHJlYW0KQlQgL0YxIDE2IFRmIDMwIDgwIFRkIChDb3Vyc2UgZXhwaXJlZCAtIHBsZWFzZSByZW5ldykgVGogRVQKZW5kc3RyZWFtCmVuZG9iago1IDAgb2JqCjw8IC9UeXBlIC9Gb250IC9TdWJ0eXBlIC9UeXBlMSAvQmFzZUZvbnQgL0hlbHZldGljYSA+PgplbmRvYmoKeHJlZgowIDYKMDAwMDAwMDAwMCA2NTUzNSBmIAowMDAwMDAwMDA5IDAwMDAwIG4gCjAwMDAwMDAwNTggMDAwMDAgbiAKMDAwMDAwMDExNSAwMDAwMCBuIAowMDAwMDAwMjQxIDAwMDAwIG4gCjAwMDAwMDAzNTAgMDAwMDAgbiAKdHJhaWxlcgo8PCAvU2l6ZSA2IC9Sb290IDEgMCBSID4+CnN0YXJ0eHJlZgo0MjAKJSVFT0YK"
	b64PlaceholderPhoto = "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"
)

type phCacheEntry struct {
	media tg.InputMediaClass
	at    time.Time
}

var (
	phMu    sync.Mutex
	phCache = map[string]phCacheEntry{}
)

func phSettingKey(kind string) string { return "expiry_placeholder_" + kind + "_logmsg" }

// getPlaceholderMedia — kind साठी dummy file चं InputMedia (ताजा file reference सह).
func getPlaceholderMedia(gctx *ext.Context, client *gotgproto.Client, kind string, force bool) (tg.InputMediaClass, error) {
	phMu.Lock()
	defer phMu.Unlock()

	if e, ok := phCache[kind]; ok && !force && time.Since(e.at) < 20*time.Minute {
		return e.media, nil
	}
	api := client.API()
	logPeer, err := utils.GetLogChannelPeer(gctx, api, client.PeerStorage)
	if err != nil || logPeer == nil {
		return nil, fmt.Errorf("LOG_CHANNEL peer मिळाला नाही: %v", err)
	}

	// 1) आधी save केलेला dummy message वापरून बघा
	if msgID := db.GetSettingInt64(phSettingKey(kind), 0); msgID != 0 {
		if media, fErr := fetchLogChannelMedia(gctx, api, logPeer, int(msgID)); fErr == nil {
			phCache[kind] = phCacheEntry{media: media, at: time.Now()}
			return media, nil
		}
	}

	// 2) नाही (किंवा message गेला) → नवीन upload करून LOG_CHANNEL मध्ये ठेवा
	var (
		data    []byte
		name    string
		upMedia func(tg.InputFileClass) tg.InputMediaClass
	)
	switch kind {
	case phVideo:
		data, _ = base64.StdEncoding.DecodeString(b64PlaceholderVideo)
		name = "course_expired.mp4"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass {
			return &tg.InputMediaUploadedDocument{
				File:     f,
				MimeType: "video/mp4",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeVideo{Duration: 1, W: 320, H: 180, SupportsStreaming: true},
					&tg.DocumentAttributeFilename{FileName: name},
				},
			}
		}
	case phPhoto:
		data, _ = base64.StdEncoding.DecodeString(b64PlaceholderPhoto)
		name = "course_expired.png"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass {
			return &tg.InputMediaUploadedPhoto{File: f}
		}
	default: // phPDF
		data, _ = base64.StdEncoding.DecodeString(b64PlaceholderPDF)
		name = "course_expired.pdf"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass {
			return &tg.InputMediaUploadedDocument{
				File:       f,
				MimeType:   "application/pdf",
				ForceFile:  true,
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}},
			}
		}
	}
	if len(data) == 0 {
		return nil, errors.New("placeholder data decode failed")
	}

	file, err := uploader.NewUploader(api).FromBytes(gctx, name, data)
	if err != nil {
		return nil, fmt.Errorf("placeholder upload failed: %w", err)
	}
	sent, err := api.MessagesSendMedia(gctx, &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: logPeer.ChannelID, AccessHash: logPeer.AccessHash},
		Media:    upMedia(file),
		Message:  "🧱 Expiry placeholder (" + kind + ") — हा message delete/बदलू नका",
		RandomID: rand.Int63(),
	})
	if err != nil {
		return nil, fmt.Errorf("placeholder send to LOG_CHANNEL failed: %w", err)
	}
	newID := extractNewMessageID(toUpdates(sent))
	if newID == 0 {
		return nil, errors.New("placeholder message ID मिळाला नाही")
	}
	_ = db.SetSettingInt64(phSettingKey(kind), int64(newID))

	media, err := fetchLogChannelMedia(gctx, api, logPeer, newID)
	if err != nil {
		return nil, err
	}
	phCache[kind] = phCacheEntry{media: media, at: time.Now()}
	return media, nil
}

// fetchLogChannelMedia — LOG_CHANNEL मधल्या message चा ताजा InputMedia.
func fetchLogChannelMedia(ctx context.Context, api *tg.Client, ch *tg.InputChannel, msgID int) (tg.InputMediaClass, error) {
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: ch,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return nil, err
	}
	cm, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(cm.Messages) == 0 {
		return nil, errors.New("placeholder message सापडला नाही")
	}
	m, ok := cm.Messages[0].(*tg.Message)
	if !ok || m.Media == nil {
		return nil, errors.New("placeholder message मध्ये media नाही")
	}
	switch mm := m.Media.(type) {
	case *tg.MessageMediaDocument:
		d, ok := mm.Document.(*tg.Document)
		if !ok {
			return nil, errors.New("placeholder document read error")
		}
		return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}, nil
	case *tg.MessageMediaPhoto:
		p, ok := mm.Photo.(*tg.Photo)
		if !ok {
			return nil, errors.New("placeholder photo read error")
		}
		return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference}}, nil
	}
	return nil, errors.New("placeholder media type unsupported")
}

// ── message चा प्रकार ओळखा ──────────────────────────────────────────────────

type msgProbe struct {
	kind string // phVideo / phPDF / phPhoto / "" (media नाही)
}

func classifyMedia(media tg.MessageMediaClass) string {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		return phPhoto
	case *tg.MessageMediaDocument:
		if d, ok := m.Document.(*tg.Document); ok {
			for _, a := range d.Attributes {
				if _, isVid := a.(*tg.DocumentAttributeVideo); isVid {
					return phVideo
				}
			}
			if strings.HasPrefix(strings.ToLower(d.MimeType), "video/") {
				return phVideo
			}
		}
		return phPDF
	}
	return ""
}

// probeMessages — bot च्या chat मधल्या IDs चा प्रकार. आता अस्तित्वात नसलेले IDs map मध्ये नसतात.
func probeMessages(ctx context.Context, api *tg.Client, ids []int) (map[int]msgProbe, error) {
	inputs := make([]tg.InputMessageClass, 0, len(ids))
	for _, id := range ids {
		inputs = append(inputs, &tg.InputMessageID{ID: id})
	}
	res, err := api.MessagesGetMessages(ctx, inputs)
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesMessages:
		msgs = r.Messages
	case *tg.MessagesMessagesSlice:
		msgs = r.Messages
	case *tg.MessagesChannelMessages:
		msgs = r.Messages
	}
	out := make(map[int]msgProbe, len(msgs))
	for _, mc := range msgs {
		if m, ok := mc.(*tg.Message); ok { // MessageEmpty = आधीच गेला
			out[m.ID] = msgProbe{kind: classifyMedia(m.Media)}
		}
	}
	return out, nil
}

// ── caption + renew बटण ─────────────────────────────────────────────────────

func expiredCaption(courseID primitive.ObjectID) (string, tg.ReplyMarkupClass) {
	title := ""
	if !courseID.IsZero() {
		if c, _ := db.GetCourseByID(courseID); c != nil {
			title = c.Title
		}
	}
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200]) + "…"
	}
	caption := "⌛ Course access expire झाला आहे."
	if title != "" {
		caption += "\n📚 " + title
	}
	caption += "\n\n🔁 हे lecture परत पाहण्यासाठी course renew करा."

	var markup tg.ReplyMarkupClass
	if !courseID.IsZero() {
		markup = &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: "🔁 Renew करा", Data: []byte("wc:course:" + courseID.Hex())},
			},
		}}}
	}
	return caption, markup
}

// ── एक message edit करून placeholder बनवा ────────────────────────────────

type editOutcome int

const (
	editReplaced editOutcome = iota // media + caption बदलला (किंवा आधीच बदललेला)
	editGone                        // message आधीच नाही
	editGiveUp                      // कधीच edit होणार नाही (Telegram ची मर्यादा)
	editRetry                       // temporary error — पुढच्या run ला
)

func replaceWithPlaceholder(gctx *ext.Context, client *gotgproto.Client, peer tg.InputPeerClass,
	msgID int, kind string, courseID primitive.ObjectID, log *zap.Logger) editOutcome {

	api := client.API()
	caption, markup := expiredCaption(courseID)

	refreshed := false
	for attempt := 0; attempt < 4; attempt++ {
		req := &tg.MessagesEditMessageRequest{Peer: peer, ID: msgID, Message: caption}
		if markup != nil {
			req.ReplyMarkup = markup
		}
		if kind != "" {
			media, err := getPlaceholderMedia(gctx, client, kind, refreshed)
			if err != nil {
				if log != nil {
					log.Warn("placeholder media मिळाला नाही", zap.String("kind", kind), zap.Error(err))
				}
				return editRetry
			}
			req.Media = media
		}

		_, err := api.MessagesEditMessage(gctx, req)
		if err == nil {
			return editReplaced
		}
		es := strings.ToUpper(err.Error())
		switch {
		case strings.Contains(es, "MESSAGE_NOT_MODIFIED"):
			return editReplaced // आधीच placeholder आहे
		case strings.Contains(es, "MESSAGE_ID_INVALID"):
			return editGone
		case strings.Contains(es, "FLOOD_WAIT"):
			secs, _ := parseFloodWait(err)
			time.Sleep(time.Duration(secs+2) * time.Second)
		case strings.Contains(es, "FILE_REFERENCE") && !refreshed:
			refreshed = true // पुढच्या loop मध्ये force refresh
		case strings.Contains(es, "MESSAGE_EDIT_TIME_EXPIRED"),
			strings.Contains(es, "MESSAGE_AUTHOR_REQUIRED"),
			strings.Contains(es, "EDIT_FORBIDDEN"),
			strings.Contains(es, "MEDIA_NEW_INVALID"),
			strings.Contains(es, "MEDIA_EMPTY"),
			strings.Contains(es, "CAN'T BE EDITED"):
			if log != nil {
				log.Warn("Message edit होऊ शकत नाही — सोडून देतोय",
					zap.Int("msg_id", msgID), zap.String("kind", kind), zap.Error(err))
			}
			return editGiveUp
		default:
			if log != nil {
				log.Warn("Placeholder edit failed — will retry later",
					zap.Int("msg_id", msgID), zap.Error(err))
			}
			return editRetry
		}
	}
	return editRetry
}

// ── मुख्य helper: आधी delete, नाही जमलं तर placeholder ─────────────────────

// deleteOrPlaceholder — ids (एकाच chat चे, ≤100) delete करतो; जे delete होत नाहीत
// त्यांचं media dummy ने + caption "renew करा" ने बदलतो.
//
//	done     = records काढायचे IDs (delete झाले / आधीच गेले / placeholder झाले / कधीच जमणार नाहीत)
//	deleted  = प्रत्यक्ष delete झालेले
//	replaced = placeholder ने बदललेले
//	retry    = temporary error — records ठेवा, पुढच्या run ला परत
//
// courseOf: message ID → course (caption/बटणासाठी). नसेल तर generic caption.
func deleteOrPlaceholder(client *gotgproto.Client, chatID int64, ids []int,
	courseOf map[int]primitive.ObjectID, log *zap.Logger) (done []int, deleted, replaced int, retry []int) {

	if len(ids) == 0 {
		return
	}
	gctx := client.CreateContext()
	api := client.API()

	// 1) delete try. (gotgproto चा DeleteMessages affected count दाखवत नाही —
	//    म्हणून थेट API; PtsCount < len म्हणजे काही प्रत्यक्ष delete झाले नाहीत.)
	var (
		fallback  = ids
		deletedOK = 0
	)
	for attempt := 0; attempt < 3; attempt++ {
		aff, dErr := api.MessagesDeleteMessages(gctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: ids})
		if dErr == nil {
			if aff != nil && aff.PtsCount >= len(ids) {
				return ids, len(ids), 0, nil // सगळे delete झाले
			}
			if aff != nil {
				deletedOK = aff.PtsCount
			}
			break // काही/सगळे राहिले → खाली fallback (गेलेले IDs MESSAGE_ID_INVALID देतील)
		}
		es := strings.ToUpper(dErr.Error())
		if strings.Contains(es, "FLOOD_WAIT") {
			secs, _ := parseFloodWait(dErr)
			time.Sleep(time.Duration(secs+2) * time.Second)
			continue
		}
		if isMsgGone(dErr) || strings.Contains(es, "CAN'T BE DELETED") {
			break // जुना message — delete शक्य नाही → fallback
		}
		if log != nil {
			log.Warn("DeleteMessages failed — will retry later", zap.Error(dErr))
		}
		return nil, 0, 0, ids
	}
	deleted = deletedOK

	// 2) fallback: कोणते IDs अजून आहेत आणि कुठल्या प्रकारचे
	peer := gctx.PeerStorage.GetInputPeerById(chatID)
	if peer.Zero() {
		return nil, deleted, 0, fallback
	}
	probe, err := probeMessages(gctx, api, fallback)
	if err != nil {
		if log != nil {
			log.Warn("probeMessages failed — will retry later", zap.Error(err))
		}
		return nil, deleted, 0, fallback
	}
	for _, id := range fallback {
		p, exists := probe[id]
		if !exists {
			done = append(done, id) // आधीच गेला
			continue
		}
		switch replaceWithPlaceholder(gctx, client, peer, id, p.kind, courseOf[id], log) {
		case editReplaced:
			replaced++
			done = append(done, id)
		case editGone, editGiveUp:
			done = append(done, id)
		case editRetry:
			retry = append(retry, id)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return
}
