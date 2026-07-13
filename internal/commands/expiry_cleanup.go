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
	"math/rand"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

func StartExpiryCleanup(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log = log.Named("expiry_cleanup")
	log.Info("Expiry-cleanup worker started — runs every 15 minutes")

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
		purgeSentLecturesForAccess(gctx, acc.UserID, acc.CourseID, log)
		_ = db.MarkAccessContentCleaned(acc.ID)
	}
}

// purgeSentLecturesForAccess — शेअर्ड helper: cmdRevokeAccess (instant manual
// revoke) आणि background worker (natural expiry) दोघेही हेच वापरतात.
// फक्त student च्या chat मधले messages delete करतो — LOG_CHANNEL cache
// (db.DeliveredCache) touch करत नाही, renew झाल्यावर तीच वापरली जाते.
func purgeSentLecturesForAccess(gctx *ext.Context, userID, courseID primitive.ObjectID, log *zap.Logger) {
	msgs, err := db.GetSentLectureMsgsForAccess(userID, courseID)
	if err != nil {
		if log != nil {
			log.Warn("GetSentLectureMsgsForAccess failed", zap.Error(err))
		}
		return
	}
	if len(msgs) == 0 {
		return
	}

	// chatID नुसार group करून batch delete (एकाच user साठी सगळे same chatID
	// असतील साधारण, तरी सुरक्षिततेसाठी group करतो).
	byChat := map[int64][]int{}
	for _, m := range msgs {
		byChat[m.ChatID] = append(byChat[m.ChatID], m.MessageID)
	}
	for chatID, ids := range byChat {
		// Telegram delete API ला एका वेळी 100 पर्यंत ठीक असतं — chunk करतो.
		for i := 0; i < len(ids); i += 100 {
			end := i + 100
			if end > len(ids) {
				end = len(ids)
			}
			if err := gctx.DeleteMessages(chatID, ids[i:end]); err != nil && log != nil {
				log.Warn("Failed to delete expired lecture messages",
					zap.Int64("chat_id", chatID), zap.Error(err))
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	_ = db.DeleteSentLectureMsgsForAccess(userID, courseID)

	// Student ला courteous notice — Buy/Renew button सकट.
	if botClient := MainBotClient; botClient != nil && len(byChat) > 0 {
		for chatID := range byChat {
			toPeer := gctx.PeerStorage.GetInputPeerById(chatID)
			if toPeer.Zero() {
				continue
			}
			_, _ = botClient.API().MessagesSendMessage(gctx, &tg.MessagesSendMessageRequest{
				Peer:     toPeer,
				Message:  "⌛ तुमचा course access expire झाला आहे — पाठवलेले lectures काढून टाकले आहेत.\n\nरिन्यू केल्यावर आधी बघितलेले lectures परत लगेच (जलद) मिळतील.",
				RandomID: rand.Int63(),
				ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
					{Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "🔁 Course बघा / Renew करा", Data: []byte("wc:course:" + courseID.Hex())},
					}},
				}},
			})
		}
	}
}
