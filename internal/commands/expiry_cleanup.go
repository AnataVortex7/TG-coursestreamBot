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
	"strings"
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

	gctx := client.CreateContext()

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

		// Access expired/revoked आहे का check करा — active असेल तर delete नाही
		accesses, err := db.ActiveAccessForUser(p.UserID)
		if err == nil {
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
		}

		// Delete attempt
		err = gctx.DeleteMessages(p.ChatID, []int{p.MessageID})
		if err != nil {
			errStr := err.Error()
			alreadyGone := strings.Contains(errStr, "MESSAGE_ID_INVALID") ||
				strings.Contains(errStr, "message not found") ||
				strings.Contains(errStr, "MESSAGE_DELETE_FORBIDDEN")

			if alreadyGone {
				// Already deleted — record काढा
				_ = db.RemovePendingDeletion(p.ID)
			} else {
				_ = db.IncrementPendingDeletionAttempt(p.ID)
				log.Warn("Pending deletion failed — will retry",
					zap.Int64("chat_id", p.ChatID),
					zap.Int("msg_id", p.MessageID),
					zap.Int("attempt", p.Attempts+1),
					zap.Error(err))
			}
		} else {
			_ = db.RemovePendingDeletion(p.ID)
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
		purgeSentLecturesForAccess(gctx, acc.UserID, acc.CourseID, log)
		// सगळ्या bots नी delete केल्यावरच cleaned mark करतो
		if pending, err := db.HasPendingSentMsgs(acc.UserID, acc.CourseID); err == nil && !pending {
			_ = db.MarkAccessContentCleaned(acc.ID)
		}
	}
}

// purgeSentLecturesForAccess — शेअर्ड helper: cmdRevokeAccess (instant manual
// revoke) आणि background worker (natural expiry) दोघेही हेच वापरतात.
// Multi-bot setup: फक्त या machine च्या bot ने पाठवलेले messages delete करतो.
// दुसऱ्या bot चे messages त्या machine च्या cleanup run मध्ये delete होतील.
// सगळ्या bots नी delete केल्यावरच access ला "cleaned" mark होतो.
func purgeSentLecturesForAccess(gctx *ext.Context, userID, courseID primitive.ObjectID, log *zap.Logger) {
	// या bot चा ID काढा
	botID := int64(0)
	if botClient := MainBotClient; botClient != nil && botClient.Self != nil {
		botID = botClient.Self.ID
	}

	msgs, err := db.GetSentLectureMsgsByBot(userID, courseID, botID)
	if err != nil {
		if log != nil {
			log.Warn("GetSentLectureMsgsByBot failed", zap.Error(err))
		}
		return
	}
	if len(msgs) == 0 {
		return
	}

	// chatID नुसार group — batch delete
	byChat := map[int64][]int{}
	for _, m := range msgs {
		byChat[m.ChatID] = append(byChat[m.ChatID], m.MessageID)
	}

	var successfulMsgIDs []int // फक्त successfully deleted IDs track करतो
	for chatID, ids := range byChat {
		for i := 0; i < len(ids); i += 100 {
			end := i + 100
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[i:end]
			if err := gctx.DeleteMessages(chatID, chunk); err != nil {
				// "message not found" = user ने आधीच delete केला → success मानतो
				errStr := err.Error()
				alreadyGone := strings.Contains(errStr, "MESSAGE_ID_INVALID") ||
					strings.Contains(errStr, "message not found") ||
					strings.Contains(errStr, "MESSAGE_DELETE_FORBIDDEN")
				if alreadyGone {
					successfulMsgIDs = append(successfulMsgIDs, chunk...)
				} else {
					if log != nil {
						log.Warn("Failed to delete expired lecture messages — will retry next run",
							zap.Int64("chat_id", chatID),
							zap.Int64("bot_id", botID),
							zap.Int("count", len(chunk)),
							zap.Error(err))
					}
				}
				time.Sleep(300 * time.Millisecond)
				continue
			}
			successfulMsgIDs = append(successfulMsgIDs, chunk...)
			time.Sleep(300 * time.Millisecond)
		}
	}

	// फक्त successfully deleted messages चे DB records काढतो
	if len(successfulMsgIDs) > 0 {
		_ = db.DeleteSentLectureMsgsByIDs(successfulMsgIDs)
	}

	// Student ला notice — फक्त successfully deleted असतील तरच
	if len(successfulMsgIDs) == 0 {
		return // सगळे fail → notice नको, next run retry करेल
	}
	successSet := make(map[int]bool, len(successfulMsgIDs))
	for _, id := range successfulMsgIDs {
		successSet[id] = true
	}
	notifiedChats := map[int64]bool{}
	if botClient := MainBotClient; botClient != nil {
		for _, m := range msgs {
			if !successSet[m.MessageID] || notifiedChats[m.ChatID] {
				continue
			}
			notifiedChats[m.ChatID] = true
			toPeer := gctx.PeerStorage.GetInputPeerById(m.ChatID)
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
