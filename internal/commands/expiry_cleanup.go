// internal/commands/expiry_cleanup.go
//
// Course access expire झाल्यावर student च्या chat मधून bot ने पाठवलेले सर्व
// lecture messages आपोआप delete करतो. LOG_CHANNEL मधली cache copy मात्र तशीच
// राहते — user ने renew केलं तर तिथूनच परत जलद (fast-path) पाठवली जाते,
// नव्याने watermark process करावी लागत नाही.
//
// StartExpiryCleanup(ctx, botClient, log) — run.go मध्ये StartJanitor
// सोबतच call करा.
package commands

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

func StartExpiryCleanup(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log = log.Named("expiry_cleanup")
	log.Info("Expiry-cleanup worker started — runs every 15 minutes")

	// pending_msg_deletions worker — old machine ने sent_lecture_msgs corrupt
	// केली तरी हा independent राहतो. Every 10 minutes retry.
	go startPendingDeletionWorker(ctx, client, log.Named("pending_del"))

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	runExpiryCleanup(client, log) // startup ला लगेच एकदा

	for {
		select {
		case <-ctx.Done():
			log.Info("Expiry-cleanup worker stopped")
			return
		case <-ticker.C:
			runExpiryCleanup(client, log)
		}
	}
}

// startPendingDeletionWorker — pending_msg_deletions independently process करतो.
// Old machine ने sent_lecture_msgs delete केल्या तरी हे records safe आहेत.
// फक्त expired/revoked access चे messages delete करतो — active access ला touch नाही.
func startPendingDeletionWorker(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log.Info("Pending-deletion worker started — runs every 10 minutes")

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	runPendingDeletionWorker(client, log)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runPendingDeletionWorker(client, log)
		}
	}
}

const maxPendingAttempts = 10 // 10 attempts नंतर give up (message manually deleted असेल)

func runPendingDeletionWorker(client *gotgproto.Client, log *zap.Logger) {
	botID := int64(0)
	if client != nil && client.Self != nil {
		botID = client.Self.ID
	}
	if botID == 0 {
		return
	}

	pending, err := db.GetPendingDeletionsForBot(botID)
	if err != nil || len(pending) == 0 {
		return
	}

	accessCache := map[primitive.ObjectID][]db.Access{} // प्रति run, user मागे एकच query
	for _, p := range pending {
		// Max attempts check — give up करतो
		if p.Attempts >= maxPendingAttempts {
			log.Warn("Giving up on pending deletion after max attempts",
				zap.Int64("chat_id", p.ChatID),
				zap.Int("msg_id", p.MessageID),
				zap.Int("attempts", p.Attempts))
			_ = db.RemovePendingDeletion(p.ID)
			continue
		}

		// Access expired/revoked आहे का check करा — active असेल तर delete नाही.
		// DB error आल्यास safe side → skip (चुकून active user चे messages delete नको).
		accesses, cached := accessCache[p.UserID]
		if !cached {
			var err error
			accesses, err = db.ActiveAccessForUser(p.UserID)
			if err != nil {
				log.Warn("Access check failed — skipping deletion for safety",
					zap.String("user_id", p.UserID.Hex()),
					zap.Error(err))
				continue
			}
			accessCache[p.UserID] = accesses
		}
		stillActive := false
		for _, a := range accesses {
			if a.CourseID == p.CourseID {
				stillActive = true
				break
			}
		}
		if stillActive {
			continue // access अजून valid आहे — delete नको
		}

		// Delete attempt — जुना message (48h+) असेल तर delete ऐवजी placeholder
		// (same dummy video/PDF/photo + "renew करा" caption) ने बदलतो.
		courseOf := map[int]primitive.ObjectID{p.MessageID: p.CourseID}
		done, _, _, retry := deleteOrPlaceholder(client, p.ChatID, []int{p.MessageID}, courseOf, log)
		if len(done) > 0 {
			_ = db.RemovePendingDeletion(p.ID)
			_ = db.DeleteSentLectureMsgsByIDs(p.UserID, done)
		} else if len(retry) > 0 {
			_ = db.IncrementPendingDeletionAttempt(p.ID)
			log.Warn("Pending deletion failed — will retry",
				zap.Int64("chat_id", p.ChatID),
				zap.Int("msg_id", p.MessageID),
				zap.Int("attempt", p.Attempts+1))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runExpiryCleanup(client *gotgproto.Client, log *zap.Logger) {
	expired, err := db.ExpiredUncleanedAccess()
	if err != nil {
		log.Error("ExpiredUncleanedAccess fetch failed", zap.Error(err))
		return
	}
	if len(expired) == 0 {
		return
	}
	log.Sugar().Infof("Expiry-cleanup: %d access record(s) to clean", len(expired))

	gctx := client.CreateContext()
	for _, acc := range expired {
		deleted, failed, err := ClearTrackedMsgs(client, acc.UserID, acc.CourseID, log)
		if err != nil {
			log.Warn("ClearTrackedMsgs failed", zap.Error(err))
			continue // पुढच्या run ला retry
		}
		if failed > 0 {
			continue // काही राहिले → cleaned mark नाही, 15 min नंतर retry
		}

		_ = db.MarkAccessContentCleaned(acc.ID)

		// काहीतरी delete झालं असेल तरच user ला notice
		if deleted == 0 {
			continue
		}
		user, uErr := db.GetUserByID(acc.UserID)
		if uErr != nil || user == nil || user.TelegramID == 0 {
			continue
		}
		toPeer := gctx.PeerStorage.GetInputPeerById(user.TelegramID)
		if toPeer.Zero() {
			continue
		}
		_, _ = client.API().MessagesSendMessage(gctx, &tg.MessagesSendMessageRequest{
			Peer:     toPeer,
			Message:  "⌛ तुमचा course access expire झाला आहे — पाठवलेले lectures काढून टाकले आहेत.\n\nRenew केल्यावर लगेच परत मिळतील.",
			RandomID: rand.Int63(),
			ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔁 Renew करा",
						Data: []byte("wc:course:" + acc.CourseID.Hex()),
					},
				}},
			}},
		})
	}
}

// ─── ClearTrackedMsgs ────────────────────────────────────────────────────────
// Bot account ला chat history वाचता येत नाही (messages.getHistory = users only),
// म्हणून bot ने स्वतः पाठवलेले messages tracking records (sent_lecture_msgs +
// pending_msg_deletions) वरून delete करतो. FloodWait handled.
//
// courseID zero (primitive.NilObjectID) → user चे सगळे courses.
// फक्त या bot चे records (message ID bot-wise असतात — दुसऱ्या bot चे ID वापरणं धोकादायक).
// Returns: deleted = chat मधून गेलेले, failed = अजून राहिलेले (retry होतील).
func ClearTrackedMsgs(client *gotgproto.Client, userID, courseID primitive.ObjectID, log *zap.Logger) (deleted, failed int, err error) {
	if client == nil || client.Self == nil {
		return 0, 0, errors.New("bot client ready नाही")
	}
	botID := client.Self.ID

	user, uErr := db.GetUserByID(userID)
	if uErr != nil || user == nil || user.TelegramID == 0 {
		return 0, 0, nil // TG link नाही → chat मध्ये काही पाठवलेलंच नाही
	}
	chatID := user.TelegramID

	// दोन्ही collections मधून unique message IDs गोळा करा
	idSet := map[int]bool{}
	courseOf := map[int]primitive.ObjectID{} // message ID → course (placeholder caption/बटणासाठी)
	if recs, e := db.GetSentLectureMsgsForUser(userID, courseID, botID); e == nil {
		for _, r := range recs {
			if r.ChatID == chatID {
				idSet[r.MessageID] = true
				courseOf[r.MessageID] = r.CourseID
			}
		}
	}
	if recs, e := db.GetPendingDeletionsForUser(userID, courseID, botID); e == nil {
		for _, r := range recs {
			if r.ChatID == chatID {
				idSet[r.MessageID] = true
				courseOf[r.MessageID] = r.CourseID
			}
		}
	}
	if len(idSet) == 0 {
		return 0, 0, nil
	}
	ids := make([]int, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	var doneIDs []int // chat मधून गेले / placeholder झाले / कधीच जमणार नाहीत → record काढायचे

	for i := 0; i < len(ids); i += 100 {
		end := i + 100
		if end > len(ids) {
			end = len(ids)
		}
		// आधी delete; 48h+ जुने असल्यामुळे delete न झालेले → dummy media + "renew करा" caption.
		// placeholder ने बदललेले पण "काढले" म्हणून मोजतो (user ला notice जावा म्हणून).
		d, del, rep, retry := deleteOrPlaceholder(client, chatID, ids[i:end], courseOf, log)
		doneIDs = append(doneIDs, d...)
		deleted += del + rep
		failed += len(retry)
		if log != nil && rep > 0 {
			log.Info("Old messages replaced with placeholder", zap.Int("deleted", del), zap.Int("replaced", rep))
		}
		time.Sleep(300 * time.Millisecond)
	}

	// फक्त यशस्वी/निरर्थक IDs चे records काढा; failed ठेवा
	_ = db.DeleteSentLectureMsgsByIDs(userID, doneIDs)
	_ = db.RemovePendingDeletionsByMsgIDs(userID, botID, doneIDs)
	return deleted, failed, nil
}

// isMsgGone — message आधीच नाही / delete करण्याची परवानगी नाही (retry होऊ शकत नाही).
func isMsgGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "MESSAGE_ID_INVALID") ||
		strings.Contains(s, "MESSAGE_DELETE_FORBIDDEN") ||
		strings.Contains(s, "message not found")
}

// parseFloodWait — FLOOD_WAIT error मधून seconds काढतो.
func parseFloodWait(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	s := err.Error()
	if !strings.Contains(s, "FLOOD_WAIT") {
		return 0, false
	}
	// "FLOOD_WAIT_42" or "FLOOD_WAIT 42"
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == ' ' || r == ':'
	}) {
		if n, e := strconv.Atoi(part); e == nil && n > 0 {
			return n, true
		}
	}
	return 5, true // default 5s
}
