// internal/db/db_offline.go
//
// OfflineToken — 5-digit token system for Telegram offline video download.
//
// Flow:
//   1. User opens course page on website → clicks "📥 Offline" on a video
//   2. Web handler calls CreateOfflineToken(userID, courseID, messageID, channelID)
//      → generates a unique 5-digit token, stores it in MongoDB (TTL: end of today IST)
//   3. User copies the token and sends `/dl <TOKEN>` in any Telegram chat with the bot
//   4. Bot calls RedeemOfflineToken(token, userID) → validates token, checks daily limit (5/day),
//      increments use_count, returns the file info
//   5. Bot fetches the file from Telegram and sends it with NoForwards=true

package db

import (
	"errors"
	"fmt"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// OfflineToken — one record per "Offline Download" button click on the website.
type OfflineToken struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"   json:"id"`
	Token      string             `bson:"token"`           // 5-digit string e.g. "48271"
	UserID     primitive.ObjectID `bson:"user_id"`         // who generated it
	CourseID   primitive.ObjectID `bson:"course_id"`       // which course (for access re-check in bot)
	MessageID  int                `bson:"message_id"`      // Telegram message id of the video
	ChannelID  int64              `bson:"channel_id"`      // Telegram channel id
	UseCount   int                `bson:"use_count"`       // how many times redeemed today
	DailyLimit int                `bson:"daily_limit"`     // always 5 (stored for clarity)
	CreatedAt  time.Time          `bson:"created_at"`
	ExpiresAt  time.Time          `bson:"expires_at"`      // TTL index auto-deletes at midnight IST
}

// ─── Daily-use tracker (separate from OfflineToken) ──────────────────────────
//
// We track per-user, per-day total downloads in a lightweight collection.
// This is separate so a user can't bypass the limit by generating multiple tokens.

// OfflineDailyUsage — one doc per (user_id, date_key).  date_key = "2006-01-02" in IST.
type OfflineDailyUsage struct {
	ID       primitive.ObjectID `bson:"_id,omitempty"`
	UserID   primitive.ObjectID `bson:"user_id"`
	DateKey  string             `bson:"date_key"`  // "2006-01-02" in IST
	Count    int                `bson:"count"`
	UpdatedAt time.Time         `bson:"updated_at"`
}

const offlineDailyMax = 5

var (
	ErrTokenNotFound    = errors.New("token not found or expired")
	ErrTokenWrongUser   = errors.New("this token belongs to a different account")
	ErrDailyLimitReached = fmt.Errorf("daily download limit of %d reached; resets at midnight IST", offlineDailyMax)
)

// ─── IST helper ──────────────────────────────────────────────────────────────

var ist *time.Location

func init() {
	var err error
	ist, err = time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// fallback: IST = UTC+5:30
		ist = time.FixedZone("IST", 5*60*60+30*60)
	}
}

// todayKeyIST returns "YYYY-MM-DD" for the current day in IST.
func todayKeyIST() string {
	return time.Now().In(ist).Format("2006-01-02")
}

// midnightIST returns today's midnight in IST (start of tomorrow = token expiry).
func midnightTomorrowIST() time.Time {
	now := time.Now().In(ist)
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, ist)
}

// ─── Token generation ─────────────────────────────────────────────────────────

// generate5DigitToken returns a random 5-digit string like "04827".
func generate5DigitToken() string {
	return fmt.Sprintf("%05d", rand.Intn(100000))
}

// ─── CreateOfflineToken ───────────────────────────────────────────────────────

// CreateOfflineToken generates a unique 5-digit token for a course video.
// Called from the web handler when user clicks "📥 Offline".
//
// Returns the token string (e.g. "48271") or an error.
func CreateOfflineToken(userID, courseID primitive.ObjectID, messageID int, channelID int64) (string, error) {
	ctx, cancel := Ctx()
	defer cancel()

	expiry := midnightTomorrowIST()

	// Try up to 10 times to get a unique token (collision is rare but possible)
	for attempt := 0; attempt < 10; attempt++ {
		tok := generate5DigitToken()

		doc := OfflineToken{
			Token:      tok,
			UserID:     userID,
			CourseID:   courseID,
			MessageID:  messageID,
			ChannelID:  channelID,
			UseCount:   0,
			DailyLimit: offlineDailyMax,
			CreatedAt:  time.Now(),
			ExpiresAt:  expiry,
		}

		_, err := ColOfflineTokens.InsertOne(ctx, doc)
		if err != nil {
			// duplicate key = token collision → retry
			if mongo.IsDuplicateKeyError(err) {
				continue
			}
			return "", err
		}
		return tok, nil
	}
	return "", errors.New("could not generate a unique token, please try again")
}

// ─── RedeemOfflineToken ───────────────────────────────────────────────────────

// RedeemResult carries the file info needed to send the video via bot.
type RedeemResult struct {
	MessageID int
	ChannelID int64
	CourseID  primitive.ObjectID
}

// RedeemOfflineToken validates the token, enforces the daily limit,
// and returns the file info so the bot can send the video.
//
// It atomically increments the daily-usage counter FIRST (before the
// token look-up succeeds) to prevent race conditions where two /dl
// commands arrive simultaneously.
func RedeemOfflineToken(tokenStr string, userID primitive.ObjectID) (*RedeemResult, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// 1. Find the token
	var tok OfflineToken
	err := ColOfflineTokens.FindOne(ctx, bson.M{"token": tokenStr}).Decode(&tok)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}

	// 2. Ownership check
	if tok.UserID != userID {
		return nil, ErrTokenWrongUser
	}

	// 3. Check + increment daily usage atomically
	dateKey := todayKeyIST()
	filter := bson.M{"user_id": userID, "date_key": dateKey}
	update := bson.M{
		"$inc":         bson.M{"count": 1},
		"$set":         bson.M{"updated_at": time.Now()},
		"$setOnInsert": bson.M{"_id": primitive.NewObjectID()},
	}
	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)

	var usage OfflineDailyUsage
	err = ColOfflineDailyUsage.FindOneAndUpdate(ctx, filter, update, opts).Decode(&usage)
	if err != nil {
		return nil, fmt.Errorf("could not update daily usage: %w", err)
	}

	if usage.Count > offlineDailyMax {
		// Roll back the increment we just did
		_, _ = ColOfflineDailyUsage.UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"count": -1}})
		return nil, ErrDailyLimitReached
	}

	// 4. Mark token as used (increment its own counter for audit)
	_, _ = ColOfflineTokens.UpdateOne(ctx,
		bson.M{"_id": tok.ID},
		bson.M{"$inc": bson.M{"use_count": 1}},
	)

	return &RedeemResult{
		MessageID: tok.MessageID,
		ChannelID: tok.ChannelID,
		CourseID:  tok.CourseID,
	}, nil
}

// ─── GetDailyUsage — for display in bot ──────────────────────────────────────

// GetDailyUsageCount returns how many downloads the user has done today (IST).
func GetDailyUsageCount(userID primitive.ObjectID) (int, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var usage OfflineDailyUsage
	err := ColOfflineDailyUsage.FindOne(ctx, bson.M{
		"user_id":  userID,
		"date_key": todayKeyIST(),
	}).Decode(&usage)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return usage.Count, nil
}
