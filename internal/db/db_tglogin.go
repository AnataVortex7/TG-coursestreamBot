// internal/db/db_tglogin.go
//
// Telegram OTP-login deep-link tokens.
//
// जेव्हा website वरचा user login करतो पण त्याचा Telegram account अजून
// link केलेला नसतो, तेव्हा एक short-lived token generate होतो आणि
// user ला "https://t.me/<bot>?start=totp_<token>" असा deep-link दिला जातो.
// User ने bot मध्ये तो उघडून /start दाबला की (internal/commands/start.go
// मधला processTelegramLoginToken) या token वरून कोणता website account आहे
// हे कळतं, telegram_id त्या account ला कायमचं link होतं, आणि लगेच OTP
// त्याच Telegram chat वर पाठवला जातो.
//
// MongoDB TTL index (expires_at) 10 मिनिटांनी record आपोआप delete करतो.
package db

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Model ────────────────────────────────────────────────────────────────────

type TGLoginToken struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	Token     string             `bson:"token"`
	UserID    primitive.ObjectID `bson:"user_id"`
	Used      bool               `bson:"used"`
	CreatedAt time.Time          `bson:"created_at"`
	ExpiresAt time.Time          `bson:"expires_at"` // TTL — MongoDB auto-deletes
}

// ─── Token generation ──────────────────────────────────────────────────────────

// generateLoginToken — 10 hex characters (crypto/rand, sहज guess होणार नाही असा).
func generateLoginToken() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ─── Queries ─────────────────────────────────────────────────────────────────

// CreateTGLoginToken — दिलेल्या website user साठी नवीन login-token बनवतो.
// जुना (न वापरलेला) token असेल तर तोही साफ करतो — एका वेळी एकच active token.
func CreateTGLoginToken(userID primitive.ObjectID) (string, error) {
	ctx, cancel := Ctx()
	defer cancel()

	_, _ = ColTGLoginTokens.DeleteMany(ctx, bson.M{"user_id": userID, "used": false})

	token, err := generateLoginToken()
	if err != nil {
		return "", err
	}

	now := time.Now()
	rec := TGLoginToken{
		ID:        primitive.NewObjectID(),
		Token:     token,
		UserID:    userID,
		Used:      false,
		CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if _, err := ColTGLoginTokens.InsertOne(ctx, rec); err != nil {
		return "", err
	}
	return token, nil
}

// GetTGLoginToken — valid (not used, not expired) token record परत देतो.
// Not found असेल तर (nil, nil).
func GetTGLoginToken(token string) (*TGLoginToken, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec TGLoginToken
	err := ColTGLoginTokens.FindOne(ctx, bson.M{
		"token":      token,
		"used":       false,
		"expires_at": bson.M{"$gt": time.Now()},
	}).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// MarkTGLoginTokenUsed — token consume झाल्यावर (link + OTP पाठवल्यावर) call करा,
// जेणेकरून तोच deep-link परत वापरता येणार नाही.
func MarkTGLoginTokenUsed(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColTGLoginTokens.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"used": true}},
	)
	return err
}

// CanCreateTGLoginToken — 30 सेकंदात एकाच user साठी वारंवार नवीन deep-link
// बनवण्यापासून rate-limit करतो (spam टाळण्यासाठी).
func CanCreateTGLoginToken(userID primitive.ObjectID) bool {
	ctx, cancel := Ctx()
	defer cancel()

	var rec TGLoginToken
	err := ColTGLoginTokens.FindOne(ctx,
		bson.M{"user_id": userID},
		options.FindOne().SetSort(bson.M{"created_at": -1}),
	).Decode(&rec)
	if err != nil {
		return true // आधी काही नाही — allow
	}
	return time.Since(rec.CreatedAt) > 30*time.Second
}
