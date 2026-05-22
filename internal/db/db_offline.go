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

type OfflineToken struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Token     string             `bson:"token"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"`
	MessageID int                `bson:"message_id"`
	ChannelID int64              `bson:"channel_id"`
	UseCount  int                `bson:"use_count"`
	CreatedAt time.Time          `bson:"created_at"`
	ExpiresAt time.Time          `bson:"expires_at"`
}

// OfflineDailyUsage — per (user_id, course_id, date_key) tracking
// Course-specific limit साठी course_id add केला
type OfflineDailyUsage struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"` // ← course-specific tracking
	DateKey   string             `bson:"date_key"`  // "2006-01-02" IST
	Count     int                `bson:"count"`
	UpdatedAt time.Time          `bson:"updated_at"`
}

// ─── Sentinel errors ─────────────────────────────────────────────────────────

const defaultDailyLimit = 3 // Course वर limit set नसेल तर हा default

var (
	ErrTokenNotFound     = errors.New("token not found or expired")
	ErrTokenWrongUser    = errors.New("this token belongs to a different account")
	ErrDailyLimitReached = errors.New("daily download limit reached; resets at midnight IST")
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

func colOffTok() *mongo.Collection   { return Database.Collection("offline_tokens") }
func colOffUsage() *mongo.Collection { return Database.Collection("offline_daily_usage") }

// ─── CreateOfflineToken ───────────────────────────────────────────────────────

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
				continue
			}
			return "", err
		}
		return tok, nil
	}
	return "", errors.New("could not generate a unique token, please try again")
}

// ─── RedeemOfflineToken ───────────────────────────────────────────────────────

type RedeemResult struct {
	MessageID     int
	ChannelID     int64
	CourseID      primitive.ObjectID
	UsedToday     int // आज किती downloads झाले (या course साठी)
	CourseLimit   int // या course चा daily limit
}

// RedeemOfflineToken — token validate करतो, course-specific daily limit enforce करतो.
// courseLimit = 0 म्हणजे default (3) वापरतो.
func RedeemOfflineToken(tokenStr string, userID primitive.ObjectID, courseLimit int) (*RedeemResult, error) {
	ctx, cancel := Ctx()
	defer cancel()

	limit := courseLimit
	if limit <= 0 {
		limit = defaultDailyLimit
	}

	// 1. Token शोधतो
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

	// 3. Per-course daily usage atomically increment
	dateKey := todayKeyIST()
	filter := bson.M{
		"user_id":   userID,
		"course_id": tok.CourseID, // ← course-specific
		"date_key":  dateKey,
	}
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

	if usage.Count > limit {
		// Rollback
		_, _ = colOffUsage().UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"count": -1}})
		return nil, ErrDailyLimitReached
	}

	// 4. Token use_count audit
	_, _ = colOffTok().UpdateOne(ctx,
		bson.M{"_id": tok.ID},
		bson.M{"$inc": bson.M{"use_count": 1}},
	)

	return &RedeemResult{
		MessageID:   tok.MessageID,
		ChannelID:   tok.ChannelID,
		CourseID:    tok.CourseID,
		UsedToday:   usage.Count,
		CourseLimit: limit,
	}, nil
}

// ─── GetDailyUsageCount ───────────────────────────────────────────────────────

// GetDailyUsageCount — user ने आज या specific course साठी किती downloads केले.
func GetDailyUsageCount(userID, courseID primitive.ObjectID) (int, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var usage OfflineDailyUsage
	err := colOffUsage().FindOne(ctx, bson.M{
		"user_id":   userID,
		"course_id": courseID,
		"date_key":  todayKeyIST(),
	}).Decode(&usage)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return usage.Count, nil
}
