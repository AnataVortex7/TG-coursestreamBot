// internal/db/db_offline.go
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

// ─── Models ──────────────────────────────────────────────────────────────────

// OfflineToken — one record per "📥 Offline" button click on the website.
type OfflineToken struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Token     string             `bson:"token"`       // 5-digit string e.g. "48271"
	UserID    primitive.ObjectID `bson:"user_id"`     // website user who generated it
	CourseID  primitive.ObjectID `bson:"course_id"`   // which course
	MessageID int                `bson:"message_id"`  // Telegram message id of the video
	ChannelID int64              `bson:"channel_id"`  // Telegram channel id
	UseCount  int                `bson:"use_count"`   // how many times redeemed (audit)
	CreatedAt time.Time          `bson:"created_at"`
	ExpiresAt time.Time          `bson:"expires_at"`  // TTL index auto-deletes at midnight IST
}

// OfflineDailyUsage — one doc per (user_id, date_key). date_key = "2006-01-02" IST.
type OfflineDailyUsage struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	DateKey   string             `bson:"date_key"` // "2006-01-02" in IST
	Count     int                `bson:"count"`
	UpdatedAt time.Time          `bson:"updated_at"`
}

// ─── Sentinel errors ─────────────────────────────────────────────────────────

const offlineDailyMax = 5

var (
	ErrTokenNotFound     = errors.New("token not found or expired")
	ErrTokenWrongUser    = errors.New("this token belongs to a different account")
	ErrDailyLimitReached = fmt.Errorf("daily download limit of %d reached; resets at midnight IST", offlineDailyMax)
)

// ─── IST timezone ────────────────────────────────────────────────────────────

var ist *time.Location

func init() {
	var err error
	ist, err = time.LoadLocation("Asia/Kolkata")
	if err != nil {
		ist = time.FixedZone("IST", 5*60*60+30*60)
	}
}

func todayKeyIST() string {
	return time.Now().In(ist).Format("2006-01-02")
}

func midnightTomorrowIST() time.Time {
	now := time.Now().In(ist)
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, ist)
}

// ─── Lazy collection accessors ───────────────────────────────────────────────
// Using functions (not package-level vars) so they are always called AFTER
// Connect() has initialised Database — zero "undefined" compile issues.

func colOffTok() *mongo.Collection    { return Database.Collection("offline_tokens") }
func colOffUsage() *mongo.Collection  { return Database.Collection("offline_daily_usage") }

// ─── CreateOfflineToken ───────────────────────────────────────────────────────

// CreateOfflineToken generates a unique 5-digit token for a course video.
// Called from the web handler when user clicks "📥 Offline".
// Returns the token string (e.g. "48271") or an error.
func CreateOfflineToken(userID, courseID primitive.ObjectID, messageID int, channelID int64) (string, error) {
	ctx, cancel := Ctx()
	defer cancel()

	expiry := midnightTomorrowIST()

	for attempt := 0; attempt < 10; attempt++ {
		tok := fmt.Sprintf("%05d", rand.Intn(100000))

		doc := OfflineToken{
			Token:     tok,
			UserID:    userID,
			CourseID:  courseID,
			MessageID: messageID,
			ChannelID: channelID,
			UseCount:  0,
			CreatedAt: time.Now(),
			ExpiresAt: expiry,
		}

		_, err := colOffTok().InsertOne(ctx, doc)
		if err != nil {
			if mongo.IsDuplicateKeyError(err) {
				continue // token collision — retry with new number
			}
			return "", err
		}
		return tok, nil
	}
	return "", errors.New("could not generate a unique token, please try again")
}

// ─── RedeemOfflineToken ───────────────────────────────────────────────────────

// RedeemResult carries the file info the bot needs to send the video.
type RedeemResult struct {
	MessageID int
	ChannelID int64
	CourseID  primitive.ObjectID
}

// RedeemOfflineToken validates token ownership, enforces 5/day limit, returns file info.
func RedeemOfflineToken(tokenStr string, userID primitive.ObjectID) (*RedeemResult, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// 1. Find token
	var tok OfflineToken
	err := colOffTok().FindOne(ctx, bson.M{"token": tokenStr}).Decode(&tok)
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

	// 3. Atomically increment daily usage counter
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
	err = colOffUsage().FindOneAndUpdate(ctx, filter, update, opts).Decode(&usage)
	if err != nil {
		return nil, fmt.Errorf("could not update daily usage: %w", err)
	}

	if usage.Count > offlineDailyMax {
		// Roll back the increment
		_, _ = colOffUsage().UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"count": -1}})
		return nil, ErrDailyLimitReached
	}

	// 4. Mark token used (audit)
	_, _ = colOffTok().UpdateOne(ctx,
		bson.M{"_id": tok.ID},
		bson.M{"$inc": bson.M{"use_count": 1}},
	)

	return &RedeemResult{
		MessageID: tok.MessageID,
		ChannelID: tok.ChannelID,
		CourseID:  tok.CourseID,
	}, nil
}

// ─── GetDailyUsageCount ───────────────────────────────────────────────────────

// GetDailyUsageCount returns how many downloads the user has done today (IST).
func GetDailyUsageCount(userID primitive.ObjectID) (int, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var usage OfflineDailyUsage
	err := colOffUsage().FindOne(ctx, bson.M{
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
