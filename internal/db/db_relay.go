// internal/db/db_relay.go
//
// Relay map persistently MongoDB मध्ये store करतो.
// Problem: sync.Map in-memory आहे — redeploy झाला की admin ला
// जुन्या messages ला reply देता येत नाही.
// Fix: forwarded message ID → original user TG ID mapping DB मध्ये save करतो.
// 7 दिवसांनी TTL index auto-delete करतो.

package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// RelayEntry — admin side forwarded message ID → original user TG ID
type RelayEntry struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"`
	ForwardedMsgID  int                `bson:"forwarded_msg_id"` // admin chat मधला message ID
	UserTelegramID  int64              `bson:"user_telegram_id"` // original user चा TG ID
	CreatedAt       time.Time          `bson:"created_at"`
	ExpiresAt       time.Time          `bson:"expires_at"` // TTL — 7 दिवसांनी auto-delete
}

func colRelay() *mongo.Collection { return Database.Collection("relay_map") }

// SaveRelayEntry — forwarded message ID आणि user TG ID save करतो.
func SaveRelayEntry(forwardedMsgID int, userTelegramID int64) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colRelay().InsertOne(ctx, RelayEntry{
		ForwardedMsgID: forwardedMsgID,
		UserTelegramID: userTelegramID,
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(7 * 24 * time.Hour),
	})
	return err
}

// GetRelayEntry — forwarded message ID वरून original user TG ID मिळवतो.
// सापडला नाही तर (0, nil) return करतो.
func GetRelayEntry(forwardedMsgID int) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var entry RelayEntry
	err := colRelay().FindOne(ctx, bson.M{
		"forwarded_msg_id": forwardedMsgID,
	}).Decode(&entry)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return entry.UserTelegramID, nil
}
