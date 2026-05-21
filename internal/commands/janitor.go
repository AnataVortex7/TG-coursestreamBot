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
	"github.com/gotd/td/tg"
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
	runDeleteJanitor(ctx, client, log)

	for {
		select {
		case <-ctx.Done():
			log.Info("Janitor stopped")
			return
		case <-ticker.C:
			runDeleteJanitor(ctx, client, log)
		}
	}
}

func runDeleteJanitor(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	dues, err := db.DueDeletes()
	if err != nil {
		log.Error("DueDeletes fetch failed", zap.Error(err))
		return
	}
	if len(dues) == 0 {
		return
	}

	log.Sugar().Infof("Janitor: %d messages to delete", len(dues))

	for _, d := range dues {
		// User च्या chat मधून message delete करतो
		_, err := client.API().MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
			ID:     []int{d.MessageID},
			Revoke: true, // both sides delete
		})
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
