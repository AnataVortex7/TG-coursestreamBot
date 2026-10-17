// internal/commands/auto_delivery.go
//
// Auto-delivery: user ne bot /start kela (kiva link kela) ki tyache sarv active courses
// folder-wise (header pinned) aapoaap pathavle jatat — LOG_CHANNEL madhla user-wise cache
// (old var /predownload ne bharlela) forward hoto. Je lecture cache madhe nahit tyanchya
// jaagi "pending" placeholder (PDF) jaato; cache aalyavar to message tyach jaagi
// editMessageMedia ne khara lecture banto. Expiry var ha sarv content aadhichya
// pending-deletion/expiry yantrane "expired" madhe edit hoto (sent_lecture_msgs + pending_msg_deletions).
//
// Env: AUTO_DELIVERY=true|false (default true)
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

	"EverythingSuckz/fsb/config"
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
	b64PendingPDF   = "JVBERi0xLjQKMSAwIG9iago8PCAvVHlwZSAvQ2F0YWxvZyAvUGFnZXMgMiAwIFIgPj4KZW5kb2JqCjIgMCBvYmoKPDwgL1R5cGUgL1BhZ2VzIC9LaWRzIFszIDAgUl0gL0NvdW50IDEgPj4KZW5kb2JqCjMgMCBvYmoKPDwgL1R5cGUgL1BhZ2UgL1BhcmVudCAyIDAgUiAvTWVkaWFCb3ggWzAgMCA0MDAgMjAwXSAvQ29udGVudHMgNCAwIFIgL1Jlc291cmNlcyA8PCAvRm9udCA8PCAvRjEgNSAwIFIgPj4gPj4gPj4KZW5kb2JqCjQgMCBvYmoKPDwgL0xlbmd0aCAxMTAgPj4Kc3RyZWFtCkJUIC9GMSAyMiBUZiA1MCAxMjAgVGQgKExlY3R1cmUgcGVuZGluZykgVGogMCAtMzAgVGQgL0YxIDE0IFRmIChJdCB3aWxsIGFycml2ZSBoZXJlIGF1dG9tYXRpY2FsbHkgc29vbi4pIFRqIEVUCmVuZHN0cmVhbQplbmRvYmoKNSAwIG9iago8PCAvVHlwZSAvRm9udCAvU3VidHlwZSAvVHlwZTEgL0Jhc2VGb250IC9IZWx2ZXRpY2EgPj4KZW5kb2JqCnhyZWYKMCA2CjAwMDAwMDAwMDAgNjU1MzUgZiAKMDAwMDAwMDAwOSAwMDAwMCBuIAowMDAwMDAwMDU4IDAwMDAwIG4gCjAwMDAwMDAxMTUgMDAwMDAgbiAKMDAwMDAwMDI0MSAwMDAwMCBuIAowMDAwMDAwNDAyIDAwMDAwIG4gCnRyYWlsZXIKPDwgL1NpemUgNiAvUm9vdCAxIDAgUiA+PgpzdGFydHhyZWYKNDcyCiUlRU9GCg=="
	b64PendingVideo = "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"
	b64PendingPhoto = "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"
)

const (
	adTickEvery   = 30 * time.Second
	adSendGap     = 1100 * time.Millisecond // ekach chat madhe ~1 msg/sec
	adPendingScan = 40                      // ekа tick madhe kiti pending items tapasayche
)

type adPHEntry struct {
	media tg.InputMediaClass
	at    time.Time
}

const (
	adSummaryEvery = 3 * time.Minute // user chya live summary madhe update (badal zalyas)
	adAdminEvery   = 20              // dar 20 lectures nantar admin la update
)

var (
	adSumMu       sync.Mutex
	adSumLastAt   = map[primitive.ObjectID]time.Time{}
	adSumLastText = map[primitive.ObjectID]string{}
	adSumState    = map[primitive.ObjectID]string{} // "progress" | "sent" | "final"
)

var (
	adPHMu    sync.Mutex
	adPHCache = map[string]adPHEntry{}
)

func adPHSettingKey(kind string) string { return "autodelivery_pending_" + kind + "_logmsg" }

// adKindFor -- lecture cha prakar (expiry sarkhach): video -> video, photo -> photo, baki -> PDF/doc.
// Placeholder ani khara lecture SAME prakaracha asto mhanun editMessageMedia kharaab hot nahi.
func adKindFor(f db.CachedFile) string {
	mt := strings.ToLower(f.MimeType)
	name := strings.ToLower(f.FileName)
	switch {
	case strings.HasPrefix(mt, "video/"),
		strings.HasSuffix(name, ".mp4"), strings.HasSuffix(name, ".mkv"), strings.HasSuffix(name, ".mov"),
		strings.HasSuffix(name, ".avi"), strings.HasSuffix(name, ".webm"), strings.HasSuffix(name, ".ts"):
		return phVideo
	case strings.HasPrefix(mt, "image/"),
		strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"), strings.HasSuffix(name, ".png"):
		return phPhoto
	}
	return phPDF
}

// StartAutoDelivery -- main bot var chalto. Ekа veli ekach tick (sequential).
func StartAutoDelivery(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log = log.Named("auto_delivery")
	if !config.ValueOf.AutoDelivery {
		log.Info("AUTO_DELIVERY=false -- auto-delivery band")
		return
	}
	db.EnsureAutoDeliveryIndexes()
	log.Info("Auto-delivery worker started")
	time.Sleep(25 * time.Second) // bot ani peer cache tayar hou dya

	t := time.NewTicker(adTickEvery)
	defer t.Stop()
	for {
		adTick(ctx, client, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func adTick(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Sugar().Errorw("auto_delivery panic recovered", "panic", r)
		}
	}()
	if client == nil || client.Self == nil {
		return
	}
	gctx := client.CreateContext()

	adResolvePending(ctx, gctx, client, log)
	adDeliverNew(ctx, gctx, client, log)
	adRefreshSummaries(gctx)
}

// ---------------------------------------------------------------- new delivery

func adDeliverNew(ctx context.Context, gctx *ext.Context, client *gotgproto.Client, log *zap.Logger) {
	users, err := db.ListAutoDeliveryUsers()
	if err != nil || len(users) == 0 {
		return
	}
	for _, au := range users {
		if ctx.Err() != nil {
			return
		}
		if blocked, _, _ := db.IsUserBlocked(au.UserID); blocked {
			continue
		}
		accesses, err := db.ActiveAccessForUser(au.UserID)
		if err != nil || len(accesses) == 0 {
			continue
		}
		if gctx.PeerStorage.GetInputPeerById(au.ChatID).Zero() {
			continue // user ne ajun bot chalu kela nahi / peer nahi
		}
		// kahi course ajun baki asel tar user chya chat madhe ek "live" progress message (3 min update)
		needWork := false
		for _, ac := range accesses {
			if !db.ADCourseDone(au.UserID, ac.CourseID) {
				needWork = true
				break
			}
		}
		if needWork {
			adEnsureSummaryMessage(gctx, &au)
		}
		for _, ac := range accesses {
			if ctx.Err() != nil {
				return
			}
			if db.ADCourseDone(au.UserID, ac.CourseID) {
				continue
			}
			db.EnsureADCourse(au.UserID, ac.CourseID)
			if ok := adDeliverCourse(ctx, gctx, client, log, au, ac.CourseID); ok {
				db.MarkADCourseDone(au.UserID, ac.CourseID)
			} else {
				return // flood / error -- pudhchya tick madhe resume (items DB madhe aahet)
			}
		}
		adUpdateSummary(gctx, au, true)
	}
}

// adDeliverCourse -- true = course purna zala. false = thamba (flood/error), nantar resume.
func adDeliverCourse(ctx context.Context, gctx *ext.Context, client *gotgproto.Client, log *zap.Logger,
	au db.ADUser, courseID primitive.ObjectID) bool {

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		return true
	}
	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		return false
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		return false
	}

	type group struct {
		name  string
		files []db.CachedFile
	}
	groups := make([]*group, 0, len(folders)+1)
	byID := make(map[primitive.ObjectID]*group, len(folders))
	for _, fo := range folders {
		g := &group{name: fo.Name}
		groups = append(groups, g)
		byID[fo.ID] = g
	}
	ungrouped := &group{name: "Ungrouped"}
	for _, f := range files {
		fo := db.FolderForSortOrder(folders, f.SortOrder)
		if fo == nil {
			ungrouped.files = append(ungrouped.files, f)
		} else if g, ok := byID[fo.ID]; ok {
			g.files = append(g.files, f)
		} else {
			ungrouped.files = append(ungrouped.files, f)
		}
	}
	if len(ungrouped.files) > 0 {
		groups = append(groups, ungrouped)
	}

	api := client.API()
	botID := client.Self.ID
	peer := gctx.PeerStorage.GetInputPeerById(au.ChatID)
	if peer.Zero() {
		return false
	}

	for gi, g := range groups {
		if len(g.files) == 0 {
			continue
		}
		// folder header (pinned) -- ekdach
		headerKey := -(gi + 1)
		if db.ClaimADItem(au.UserID, courseID, headerKey, au.ChatID) {
			text := fmt.Sprintf("📁 %s\n%s", g.name, course.Title)
			var hid int
			if err := adWithFlood(ctx, func() error {
				upd, e := api.MessagesSendMessage(gctx, &tg.MessagesSendMessageRequest{
					Peer: peer, Message: text, RandomID: rand.Int63(),
				})
				hid = adSentID(upd)
				return e
			}); err != nil {
				db.ReleaseADItem(au.UserID, courseID, headerKey)
				log.Warn("header send failed", zap.Error(err))
				return false
			}
			if hid != 0 {
				_ = db.RecordSentLectureMsg(au.UserID, courseID, au.ChatID, hid, botID)
				_ = db.AddPendingDeletion(au.UserID, courseID, au.ChatID, hid, botID)
				_ = db.SaveADItem(db.ADItem{UserID: au.UserID, CourseID: courseID, MessageID: headerKey,
					ChatID: au.ChatID, SentMsgID: hid, State: db.ADStateHeader, Title: g.name})
				// silent pin (notification nahi). Error ignore -- pin limit/permission.
				_, _ = api.MessagesUpdatePinnedMessage(gctx, &tg.MessagesUpdatePinnedMessageRequest{
					Peer: peer, ID: hid, Silent: true,
				})
			} else {
				_ = db.SaveADItem(db.ADItem{UserID: au.UserID, CourseID: courseID, MessageID: headerKey,
					ChatID: au.ChatID, State: db.ADStateHeader, Title: g.name})
			}
			time.Sleep(adSendGap)
		}

		for _, f := range g.files {
			if ctx.Err() != nil {
				return false
			}
			if !db.ClaimADItem(au.UserID, courseID, f.MessageID, au.ChatID) {
				continue // aadhich zala kiva dusra instance pathavtoy
			}
			title := f.Title
			if title == "" {
				title = f.FileName
			}

			var sentID int
			state := db.ADStatePending
			if c, cErr := db.GetDeliveredCache(au.UserID, courseID, f.MessageID); cErr == nil && c != nil && c.LogChannelMsgID != 0 {
				// cached -> LOG_CHANNEL madhun forward (watermark/process nahi)
				fErr := adWithFlood(ctx, func() error {
					upd, e := utils.ForwardFromLogChannel(gctx, c.LogChannelMsgID, au.ChatID)
					if e == nil {
						sentID = extractNewMessageID(upd)
					}
					return e
				})
				if fErr == nil {
					state = db.ADStateDelivered
				} else if strings.Contains(strings.ToUpper(fErr.Error()), "FLOOD") {
					db.ReleaseADItem(au.UserID, courseID, f.MessageID)
					return false
				}
			}
			if state != db.ADStateDelivered {
				// [NEW] watermarked copy ajun nahi -- rikama placeholder aivaji original
				// (watermark nasleli) copy lagech pathavto; watermarked copy cache madhe aali
				// ki adResolvePending ti tyach message var replace karto.
				rID, _, rErr := adSendOriginal(ctx, gctx, client, peer, course.ChannelID, f.ChannelID, f.MessageID, title)
				switch {
				case rErr == nil && rID != 0:
					sentID = rID
					state = db.ADStateRaw
				case rErr != nil && strings.Contains(strings.ToUpper(fmtErr(rErr)), "FLOOD"):
					db.ReleaseADItem(au.UserID, courseID, f.MessageID)
					return false
				case rErr != nil:
					log.Warn("original copy send failed -- placeholder fallback", zap.Error(rErr))
				}
			}
			if state != db.ADStateDelivered && state != db.ADStateRaw {
				// pending placeholder (PDF) -- fakt original sapadli nahi tarach
				media, mErr := adPendingMedia(gctx, client, adKindFor(f))
				if mErr != nil {
					db.ReleaseADItem(au.UserID, courseID, f.MessageID)
					log.Warn("pending placeholder media nahi", zap.Error(mErr))
					return false
				}
				caption := "⏳ " + title + "\n\nहा lecture लवकरच इथेच येईल — काही करायची गरज नाही."
				if err := adWithFlood(ctx, func() error {
					upd, e := api.MessagesSendMedia(gctx, &tg.MessagesSendMediaRequest{
						Peer: peer, Media: media, Message: caption, RandomID: rand.Int63(),
					})
					sentID = adSentID(upd)
					return e
				}); err != nil {
					db.ReleaseADItem(au.UserID, courseID, f.MessageID)
					log.Warn("pending send failed", zap.Error(err))
					return false
				}
			}
			if sentID != 0 {
				_ = db.RecordSentLectureMsg(au.UserID, courseID, au.ChatID, sentID, botID)
				_ = db.AddPendingDeletion(au.UserID, courseID, au.ChatID, sentID, botID)
			} else if state == db.ADStatePending {
				state = db.ADStateGone // message ID milala nahi -> pudhe edit shakya nahi
			}
			_ = db.SaveADItem(db.ADItem{UserID: au.UserID, CourseID: courseID, MessageID: f.MessageID,
				ChatID: au.ChatID, SentMsgID: sentID, State: state, Title: title})
			adNoteProgress(gctx, au.UserID, course.Title, state)
			time.Sleep(adSendGap)
		}
	}
	return true
}

// adSendOriginal -- source channel madhla original (watermark nasleli) lecture user chya chat madhe
// pathavto (file re-upload nahi, fakt file reference). Noforwards=true (forward/save band).
// title khali asel tar source message cha caption vaprto. Return: navin message ID + vaprlela caption.
func adSendOriginal(ctx context.Context, gctx *ext.Context, client *gotgproto.Client, peer tg.InputPeerClass,
	courseChannelID, fileChannelID int64, messageID int, title string) (int, string, error) {

	chID := fileChannelID
	if chID == 0 {
		chID = courseChannelID
	}
	if chID == 0 {
		return 0, "", errors.New("source channel mahit nahi")
	}
	api := client.API()
	ch, err := utils.GetChannelPeer(gctx, api, client.PeerStorage, chID)
	if err != nil || ch == nil {
		return 0, "", fmt.Errorf("source channel peer nahi: %v", err)
	}
	media, srcCaption, ents, err := adFetchLogMedia(gctx, api, ch, messageID)
	if err != nil {
		return 0, "", err
	}
	caption := title
	var entities []tg.MessageEntityClass
	if caption == "" {
		caption, entities = srcCaption, ents
	}
	var sentID int
	err = adWithFlood(ctx, func() error {
		upd, e := api.MessagesSendMedia(gctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Media: media, Message: caption, Entities: entities,
			Noforwards: true, RandomID: rand.Int63(),
		})
		sentID = adSentID(upd)
		return e
	})
	if err != nil {
		return 0, "", err
	}
	return sentID, caption, nil
}

// ---------------------------------------------------------------- pending -> lecture

func adResolvePending(ctx context.Context, gctx *ext.Context, client *gotgproto.Client, log *zap.Logger) {
	items, err := db.ListPendingADItems(adPendingScan)
	if err != nil || len(items) == 0 {
		return
	}
	api := client.API()
	botID := client.Self.ID
	logPeer, lErr := utils.GetLogChannelPeer(gctx, api, client.PeerStorage)
	if lErr != nil || logPeer == nil {
		return
	}
	accessOK := map[primitive.ObjectID]map[primitive.ObjectID]bool{}

	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		m, ok := accessOK[it.UserID]
		if !ok {
			m = map[primitive.ObjectID]bool{}
			if acs, e := db.ActiveAccessForUser(it.UserID); e == nil {
				for _, a := range acs {
					m[a.CourseID] = true
				}
			} else {
				continue
			}
			accessOK[it.UserID] = m
		}
		if !m[it.CourseID] {
			db.SetADItemState(it.ID, db.ADStateExpired, 0) // expiry yantra already edit karto
			continue
		}
		// Fresh (DB) read -- memory cache chi 15 min "not found" nondh bypass
		c, cErr := db.GetDeliveredCacheFresh(it.UserID, it.CourseID, it.MessageID)
		if cErr != nil || c == nil || c.LogChannelMsgID == 0 {
			db.TouchADItem(it.ID) // ajun cache nahi -- nantar
			continue
		}
		peer := gctx.PeerStorage.GetInputPeerById(it.ChatID)
		if peer.Zero() {
			db.TouchADItem(it.ID)
			continue
		}

		media, caption, ents, fErr := adFetchLogMedia(gctx, api, logPeer, c.LogChannelMsgID)
		if fErr != nil {
			log.Warn("replace: LOG_CHANNEL media vachta aala nahi", zap.String("user", it.UserID.Hex()),
				zap.Int("lecture", it.MessageID), zap.Int("logMsg", c.LogChannelMsgID), zap.Error(fErr))
			db.TouchADItem(it.ID)
			continue
		}
		editErr := adWithFlood(ctx, func() error {
			_, e := api.MessagesEditMessage(gctx, &tg.MessagesEditMessageRequest{
				Peer: peer, ID: it.SentMsgID, Media: media, Message: caption, Entities: ents,
			})
			return e
		})
		switch {
		case editErr == nil || strings.Contains(strings.ToUpper(fmtErr(editErr)), "MESSAGE_NOT_MODIFIED"):
			log.Info("replace: watermarked copy ne edit zala", zap.String("user", it.UserID.Hex()),
				zap.Int("lecture", it.MessageID), zap.Int("sentMsg", it.SentMsgID), zap.String("from", it.State))
			db.SetADItemState(it.ID, db.ADStateDelivered, 0)
			adNoteProgress(gctx, it.UserID, "", db.ADStateDelivered)
		case isMsgGone(editErr):
			db.SetADItemState(it.ID, db.ADStateGone, 0) // user ne placeholder delete kela
		case strings.Contains(strings.ToUpper(fmtErr(editErr)), "FLOOD"):
			return
		default:
			// Edit shakya nahi (media type/ref issue) -> placeholder delete + lecture forward (shevti)
			log.Warn("pending edit failed -- fallback delete+forward", zap.Error(editErr))
			_, _ = api.MessagesDeleteMessages(gctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{it.SentMsgID}})
			var newID int
			if e := adWithFlood(ctx, func() error {
				upd, e2 := utils.ForwardFromLogChannel(gctx, c.LogChannelMsgID, it.ChatID)
				if e2 == nil {
					newID = extractNewMessageID(upd)
				}
				return e2
			}); e == nil && newID != 0 {
				_ = db.RecordSentLectureMsg(it.UserID, it.CourseID, it.ChatID, newID, botID)
				_ = db.AddPendingDeletion(it.UserID, it.CourseID, it.ChatID, newID, botID)
				db.SetADItemState(it.ID, db.ADStateDelivered, newID)
				adNoteProgress(gctx, it.UserID, "", db.ADStateDelivered)
			} else {
				db.TouchADItem(it.ID)
			}
		}
		time.Sleep(adSendGap)
	}
}

// ---------------------------------------------------------------- helpers

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// adWithFlood -- FLOOD_WAIT var thamblo ani parat prayatna (max 3 veLa, <=300s).
func adWithFlood(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		secs, isFlood := parseFloodWait(err)
		if !isFlood {
			return err
		}
		if secs > 300 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(secs+1) * time.Second):
		}
	}
	return err
}

// adSentID -- send/sendMedia response madhun navin message ID (private chat madhe UpdateShortSentMessage yeto).
func adSentID(upd tg.UpdatesClass) int {
	switch u := upd.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID
	case *tg.Updates:
		return extractNewMessageID(u)
	case *tg.UpdatesCombined:
		return extractNewMessageID(&tg.Updates{Updates: u.Updates})
	}
	return 0
}

// adFetchLogMedia -- LOG_CHANNEL message -> (InputMedia, caption, entities).
func adFetchLogMedia(ctx context.Context, api *tg.Client, ch *tg.InputChannel, msgID int) (tg.InputMediaClass, string, []tg.MessageEntityClass, error) {
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: ch, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return nil, "", nil, err
	}
	cm, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(cm.Messages) == 0 {
		return nil, "", nil, errors.New("log message sapadla nahi")
	}
	m, ok := cm.Messages[0].(*tg.Message)
	if !ok || m.Media == nil {
		return nil, "", nil, errors.New("log message madhe media nahi")
	}
	switch mm := m.Media.(type) {
	case *tg.MessageMediaDocument:
		d, ok := mm.Document.(*tg.Document)
		if !ok {
			return nil, "", nil, errors.New("document read error")
		}
		return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}},
			m.Message, m.Entities, nil
	case *tg.MessageMediaPhoto:
		p, ok := mm.Photo.(*tg.Photo)
		if !ok {
			return nil, "", nil, errors.New("photo read error")
		}
		return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference}},
			m.Message, m.Entities, nil
	}
	return nil, "", nil, errors.New("media type unsupported")
}

// adPendingMedia -- "pending" placeholder cha InputMedia (kind = video/pdf/photo).
// Pahilyandach LOG_CHANNEL madhe upload, message ID app_settings madhe save; nantar tochi reuse
// (expiry placeholder sarkha).
func adPendingMedia(gctx *ext.Context, client *gotgproto.Client, kind string) (tg.InputMediaClass, error) {
	adPHMu.Lock()
	defer adPHMu.Unlock()
	if e, ok := adPHCache[kind]; ok && time.Since(e.at) < 20*time.Minute {
		return e.media, nil
	}
	api := client.API()
	logPeer, err := utils.GetLogChannelPeer(gctx, api, client.PeerStorage)
	if err != nil || logPeer == nil {
		return nil, fmt.Errorf("LOG_CHANNEL peer nahi: %v", err)
	}
	if id := db.GetSettingInt64(adPHSettingKey(kind), 0); id != 0 {
		if media, _, _, fErr := adFetchLogMedia(gctx, api, logPeer, int(id)); fErr == nil {
			adPHCache[kind] = adPHEntry{media: media, at: time.Now()}
			return media, nil
		}
	}

	var (
		b64     string
		name    string
		upMedia func(tg.InputFileClass) tg.InputMediaClass
	)
	switch kind {
	case phVideo:
		b64, name = b64PendingVideo, "lecture_pending.mp4"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass {
			return &tg.InputMediaUploadedDocument{
				File: f, MimeType: "video/mp4",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeVideo{Duration: 1, W: 320, H: 180, SupportsStreaming: true},
					&tg.DocumentAttributeFilename{FileName: name},
				},
			}
		}
	case phPhoto:
		b64, name = b64PendingPhoto, "lecture_pending.png"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass { return &tg.InputMediaUploadedPhoto{File: f} }
	default: // phPDF
		b64, name = b64PendingPDF, "lecture_pending.pdf"
		upMedia = func(f tg.InputFileClass) tg.InputMediaClass {
			return &tg.InputMediaUploadedDocument{
				File: f, MimeType: "application/pdf", ForceFile: true,
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}},
			}
		}
	}
	data, _ := base64.StdEncoding.DecodeString(b64)
	if len(data) == 0 {
		return nil, errors.New("pending placeholder decode failed")
	}
	file, err := uploader.NewUploader(api).FromBytes(gctx, name, data)
	if err != nil {
		return nil, fmt.Errorf("pending placeholder upload failed: %w", err)
	}
	sent, err := api.MessagesSendMedia(gctx, &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: logPeer.ChannelID, AccessHash: logPeer.AccessHash},
		Media:    upMedia(file),
		Message:  "Auto-delivery pending placeholder (" + kind + ") -- he message delete karu naka",
		RandomID: rand.Int63(),
	})
	if err != nil {
		return nil, fmt.Errorf("pending placeholder send to LOG failed: %w", err)
	}
	newID := adSentID(sent)
	if newID == 0 {
		return nil, errors.New("pending placeholder message ID sapadla nahi")
	}
	_ = db.SetSettingInt64(adPHSettingKey(kind), int64(newID))
	media, _, _, err := adFetchLogMedia(gctx, api, logPeer, newID)
	if err != nil {
		return nil, err
	}
	adPHCache[kind] = adPHEntry{media: media, at: time.Now()}
	return media, nil
}

// ---------------------------------------------------------------- live summary + admin updates

func adUserLabel(userID primitive.ObjectID) string {
	u, err := db.GetUserByID(userID)
	if err != nil || u == nil {
		return userID.Hex()
	}
	name := u.FullName
	if name == "" {
		name = u.Username
	}
	if u.Mobile != "" {
		return fmt.Sprintf("%s (%s)", name, u.Mobile)
	}
	return name
}

func adNotifyAdmins(gctx *ext.Context, text string) {
	for _, id := range config.ValueOf.AllowedUsers {
		if _, err := gctx.SendMessage(id, &tg.MessagesSendMessageRequest{Message: text}); err != nil {
			continue
		}
	}
}

// adNoteProgress -- pratyek lecture (pathavlela kiva pending-> aalela) nantar counter ++ ;
// dar 20 vyaa lecture la admin la "kontya user che kiti aale" update.
func adNoteProgress(gctx *ext.Context, userID primitive.ObjectID, courseTitle, state string) {
	n := db.IncADSentCount(userID)
	if n == 0 || n%adAdminEvery != 0 {
		return
	}
	d, p := 0, 0
	for _, ac := range db.ListADCoursesForUser(userID) {
		dd, pp := db.CountADItems(userID, ac.CourseID)
		d += dd
		p += pp
	}
	adNotifyAdmins(gctx, fmt.Sprintf("📊 Auto-delivery update\n👤 %s\n✅ %d lectures पोहोचले | ⏳ %d pending\n(एकूण हालचाल: %d)",
		adUserLabel(userID), d, p, n))
}

// adSummaryText -- user cha sadhyacha chitra + state ("progress" / "sent" / "final").
func adSummaryText(userID primitive.ObjectID) (string, string) {
	courses := db.ListADCoursesForUser(userID)
	if len(courses) == 0 {
		return "", ""
	}
	var lines []string
	totalD, totalP := 0, 0
	allDone := true
	for _, ac := range courses {
		title := "Course"
		if c, _ := db.GetCourseByID(ac.CourseID); c != nil {
			title = c.Title
		}
		d, p := db.CountADItems(userID, ac.CourseID)
		totalD += d
		totalP += p
		switch {
		case !ac.Done:
			allDone = false
			lines = append(lines, fmt.Sprintf("• %s — 🔄 चालू आहे (✅ %d, ⏳ %d)", title, d, p))
		case p > 0:
			lines = append(lines, fmt.Sprintf("• %s — ✅ %d पाठवले, ⏳ %d येत आहेत", title, d, p))
		default:
			lines = append(lines, fmt.Sprintf("• %s — ✅ %d पाठवले (पूर्ण)", title, d))
		}
	}
	state := "progress"
	head := "📦 Auto-delivery सुरू आहे... कृपया थांबा"
	switch {
	case allDone && totalP == 0:
		state, head = "final", "✅ सगळे lectures आले! Auto-delivery पूर्ण"
	case allDone:
		state, head = "sent", "📦 सगळे courses पाठवले — काही lectures अजून तयार होत आहेत"
	}
	text := head + "\n\n" + strings.Join(lines, "\n") + fmt.Sprintf("\n\nएकूण: ✅ %d | ⏳ %d", totalD, totalP)
	if totalP > 0 {
		text += "\n\n⏳ pending lectures तयार झाल्यावर त्याच जागी आपोआप बदलतील आणि हा message दर काही मिनिटांनी अपडेट होईल."
	}
	if state == "final" {
		text += "\n\nChat clear केली तरी Course → Download बटणाने परत मिळवू शकता."
	}
	return text, state
}

// adEnsureSummaryMessage -- batch chya suruvatila ek live message pathavto (jar nasel / junya batch cha final zala asel).
func adEnsureSummaryMessage(gctx *ext.Context, au *db.ADUser) {
	cur := db.GetADUser(au.UserID)
	if cur != nil && cur.SummaryMsgID != 0 && !cur.SummaryFinal {
		au.SummaryMsgID, au.SummaryFinal = cur.SummaryMsgID, cur.SummaryFinal
		return
	}
	msg, err := gctx.SendMessage(au.ChatID, &tg.MessagesSendMessageRequest{
		Message: "📦 Auto-delivery सुरू आहे... कृपया थांबा\n\nहा message दर काही मिनिटांनी अपडेट होईल.",
	})
	if err != nil || msg == nil {
		return
	}
	if !db.TrySetADSummaryMsg(au.UserID, msg.ID) {
		// dusrya instance ne aadhich banavla -- aaplа message delete
		_, _ = gctx.Raw.MessagesDeleteMessages(gctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{msg.ID}})
		return
	}
	au.SummaryMsgID, au.SummaryFinal = msg.ID, false
	adSumMu.Lock()
	delete(adSumLastText, au.UserID)
	adSumState[au.UserID] = "progress"
	adSumLastAt[au.UserID] = time.Time{}
	adSumMu.Unlock()
	adNotifyAdmins(gctx, fmt.Sprintf("▶️ Auto-delivery सुरू\n👤 %s", adUserLabel(au.UserID)))
}

// adUpdateSummary -- live message edit (fakt text badalla asel tarach; 3 min throttle; force=true sodun).
func adUpdateSummary(gctx *ext.Context, au db.ADUser, force bool) {
	cur := db.GetADUser(au.UserID)
	if cur == nil || cur.SummaryMsgID == 0 || cur.SummaryFinal {
		return
	}
	adSumMu.Lock()
	last := adSumLastAt[au.UserID]
	prevText := adSumLastText[au.UserID]
	prevState := cur.SummaryState // DB madhla -- restart nantar duplicate admin msg nahi
	adSumMu.Unlock()
	if !force && time.Since(last) < adSummaryEvery {
		return
	}
	text, state := adSummaryText(au.UserID)
	if text == "" {
		return
	}
	if text == prevText && state == prevState {
		adSumMu.Lock()
		adSumLastAt[au.UserID] = time.Now()
		adSumMu.Unlock()
		return
	}
	peer := gctx.PeerStorage.GetInputPeerById(au.ChatID)
	if peer.Zero() {
		return
	}
	_, err := gctx.Raw.MessagesEditMessage(gctx, &tg.MessagesEditMessageRequest{
		Peer: peer, ID: cur.SummaryMsgID, Message: text,
	})
	if err != nil && !strings.Contains(strings.ToUpper(fmtErr(err)), "MESSAGE_NOT_MODIFIED") {
		if isMsgGone(err) {
			db.SetADSummaryFinal(au.UserID) // user ne message delete kela -- update thambva
		}
		return
	}
	adSumMu.Lock()
	adSumLastAt[au.UserID] = time.Now()
	adSumLastText[au.UserID] = text
	adSumState[au.UserID] = state
	adSumMu.Unlock()
	if state != prevState {
		db.SetADSummaryState(au.UserID, state)
	}

	if state != prevState {
		switch state {
		case "sent":
			adNotifyAdmins(gctx, fmt.Sprintf("📤 सगळे courses पाठवले (काही pending)\n👤 %s\n%s", adUserLabel(au.UserID), text))
		case "final":
			adNotifyAdmins(gctx, fmt.Sprintf("✅ Auto-delivery पूर्ण\n👤 %s\n%s", adUserLabel(au.UserID), text))
		}
	}
	if state == "final" {
		db.SetADSummaryFinal(au.UserID)
	}
}

// adRefreshSummaries -- delivery nasli tari (pending -> aale) dar 3 min la live message update.
func adRefreshSummaries(gctx *ext.Context) {
	users, err := db.ListAutoDeliveryUsers()
	if err != nil {
		return
	}
	for _, au := range users {
		if au.SummaryMsgID == 0 || au.SummaryFinal {
			continue
		}
		adUpdateSummary(gctx, au, false)
	}
}
