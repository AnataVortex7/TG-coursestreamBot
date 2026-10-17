// internal/commands/janitor.go
//
// Background worker — दर 10 मिनिटांनी run होतो:
//   1. DueDeletes() — 48h expire झालेले messages delete करतो
//
// StartJanitor(ctx, botClient) — run.go मध्ये call करा (bot start झाल्यावर)

package commands

import (
	"context"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto"
	"go.uber.org/zap"
)

// StartJanitor — goroutine म्हणून start करा.
// ctx cancel झाला की janitor बंद होतो.
func StartJanitor(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log = log.Named("janitor")
	log.Info("Janitor started — runs every 10 minutes")

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	// Startup ला लगेच एकदा run करा
	runDeleteJanitor(client, log)

	for {
		select {
		case <-ctx.Done():
			log.Info("Janitor stopped")
			return
		case <-ticker.C:
			runDeleteJanitor(client, log)
		}
	}
}

func runDeleteJanitor(client *gotgproto.Client, log *zap.Logger) {
	dues, err := db.DueDeletes()
	if err != nil {
		log.Error("DueDeletes fetch failed", zap.Error(err))
		return
	}
	if len(dues) == 0 {
		return
	}

	log.Sugar().Infof("Janitor: %d messages to delete", len(dues))

	// gotgproto context — chatID सहित DeleteMessages साठी
	gctx := client.CreateContext()

	for _, d := range dues {
		// ctx.DeleteMessages(chatID, msgIDs) — user च्या chat मधून correctly delete करतो
		err := gctx.DeleteMessages(d.ChatID, []int{d.MessageID})
		if err != nil {
			log.Warn("Failed to delete message",
				zap.Int64("chat_id", d.ChatID),
				zap.Int("msg_id", d.MessageID),
				zap.Error(err),
			)
			// Error आला तरी MarkDeleted करतो — infinite retry नको
		}
		_ = db.MarkDeleted(d.ID)
		log.Sugar().Infof("Deleted msg %d from chat %d", d.MessageID, d.ChatID)

		// Rate limit avoid — 500ms gap
		time.Sleep(500 * time.Millisecond)
	}
}

// ClearScheduledChatMsgs — user permanent delete karnyaadhi: ya chat madhle schedule-delete
// (pending_deletes) che ajun baki bot messages lagech chat madhun kadhto (best-effort).
// Returns: kiti messages delete zhale.
func ClearScheduledChatMsgs(client *gotgproto.Client, chatID int64) int {
	if client == nil || client.Self == nil || chatID == 0 {
		return 0
	}
	list, err := db.PendingDeletesForChat(chatID)
	if err != nil || len(list) == 0 {
		return 0
	}
	gctx := client.CreateContext()
	deleted := 0
	for i := 0; i < len(list); i += 100 {
		end := i + 100
		if end > len(list) {
			end = len(list)
		}
		ids := make([]int, 0, end-i)
		for _, d := range list[i:end] {
			ids = append(ids, d.MessageID)
		}
		if err := gctx.DeleteMessages(chatID, ids); err == nil {
			deleted += len(ids)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return deleted
}
