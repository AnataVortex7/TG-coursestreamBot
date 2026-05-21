// internal/db/db_guard.go
//
// Guard system — दोन features:
//
// 1. AUTO-DELETE: Bot ने पाठवलेला video message 2 दिवसांनी आपोआप delete होतो.
//    Bot message पाठवल्यावर PendingDelete record store करतो.
//    Background worker दर 10 मिनिटांनी expired records check करतो आणि delete करतो.
//
// 2. SUSPICIOUS ACTIVITY: User च्या download pattern वर नजर ठेवतो.
//    Score जास्त झाला तर admin ला alert येतो.
//    Admin /blockuser command ने user ला block करू शकतो.

package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Models ──────────────────────────────────────────────────────────────────

// PendingDelete — bot ने पाठवलेला प्रत्येक video message track करतो.
// Background worker हे 2 दिवसांनी delete करतो.
type PendingDelete struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	ChatID     int64              `bson:"chat_id"`      // user चा Telegram chat ID
	MessageID  int                `bson:"message_id"`   // bot ने पाठवलेल्या message चा ID
	DeleteAt   time.Time          `bson:"delete_at"`    // कधी delete करायचा (now + 48h)
	CreatedAt  time.Time          `bson:"created_at"`
	Deleted    bool               `bson:"deleted"`      // already deleted?
}

// UserGuard — per-user suspicion tracking + block status
type UserGuard struct {
	ID                  primitive.ObjectID `bson:"_id,omitempty"`
	UserID              primitive.ObjectID `bson:"user_id"`
	SuspicionScore      int                `bson:"suspicion_score"`       // cumulative score
	ConsecutiveFullDays int                `bson:"consecutive_full_days"` // days with 5/5 downloads
	LastFullDay         string             `bson:"last_full_day"`         // "2006-01-02" IST
	LastAlertedAt       time.Time          `bson:"last_alerted_at"`       // admin ला last alert कधी गेला
	IsBlocked           bool               `bson:"is_blocked"`
	BlockedUntil        time.Time          `bson:"blocked_until"`
	BlockReason         string             `bson:"block_reason"`
	UpdatedAt           time.Time          `bson:"updated_at"`
}

// ─── Lazy collection accessors ────────────────────────────────────────────────

func colPendingDeletes() *mongo.Collection { return Database.Collection("pending_deletes") }
func colUserGuard() *mongo.Collection      { return Database.Collection("user_guard") }

// ─── AUTO-DELETE helpers ──────────────────────────────────────────────────────

// ScheduleDelete — bot ने video पाठवल्यावर लगेच call करा.
// chatID = user चा TG ID, messageID = bot ने पाठवलेल्या message चा ID.
func ScheduleDelete(chatID int64, messageID int) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colPendingDeletes().InsertOne(ctx, PendingDelete{
		ChatID:    chatID,
		MessageID: messageID,
		DeleteAt:  time.Now().Add(48 * time.Hour),
		CreatedAt: time.Now(),
		Deleted:   false,
	})
	return err
}

// DueDeletes — delete_at < now आणि deleted=false असलेले records return करतो.
// Background worker हे call करतो.
func DueDeletes() ([]PendingDelete, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := colPendingDeletes().Find(ctx, bson.M{
		"delete_at": bson.M{"$lte": time.Now()},
		"deleted":   false,
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []PendingDelete
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// MarkDeleted — worker ने message delete केल्यावर record update करतो.
func MarkDeleted(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colPendingDeletes().UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"deleted": true}},
	)
	return err
}

// ─── BLOCK CHECK ─────────────────────────────────────────────────────────────

// IsUserBlocked — /dl command च्या आधी check करा.
// Returns (blocked bool, blockedUntil time, reason string)
func IsUserBlocked(userID primitive.ObjectID) (bool, time.Time, string) {
	ctx, cancel := Ctx()
	defer cancel()

	var g UserGuard
	err := colUserGuard().FindOne(ctx, bson.M{"user_id": userID}).Decode(&g)
	if err != nil {
		return false, time.Time{}, ""
	}
	if !g.IsBlocked {
		return false, time.Time{}, ""
	}
	// Block expire झाला का?
	if time.Now().After(g.BlockedUntil) {
		// Auto-unblock
		_, _ = colUserGuard().UpdateOne(ctx,
			bson.M{"user_id": userID},
			bson.M{"$set": bson.M{"is_blocked": false, "block_reason": ""}},
		)
		return false, time.Time{}, ""
	}
	return true, g.BlockedUntil, g.BlockReason
}

// ─── MANUAL BLOCK / UNBLOCK ──────────────────────────────────────────────────

// BlockUser — admin command ने user ला block करतो.
func BlockUser(userID primitive.ObjectID, days int, reason string) error {
	ctx, cancel := Ctx()
	defer cancel()

	until := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	_, err := colUserGuard().UpdateOne(ctx,
		bson.M{"user_id": userID},
		bson.M{"$set": bson.M{
			"is_blocked":    true,
			"blocked_until": until,
			"block_reason":  reason,
			"updated_at":    time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}

// UnblockUser — admin command ने user ला unblock करतो.
func UnblockUser(userID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colUserGuard().UpdateOne(ctx,
		bson.M{"user_id": userID},
		bson.M{"$set": bson.M{
			"is_blocked":   false,
			"block_reason": "",
			"updated_at":   time.Now(),
		}},
	)
	return err
}

// ─── SUSPICIOUS ACTIVITY TRACKING ────────────────────────────────────────────

// SuspicionThreshold — हा score ओलांडला की admin ला alert जातो.
const SuspicionThreshold = 15

// RecordDownload — प्रत्येक successful /dl नंतर call करा.
// Returns (shouldAlert bool, currentScore int)
// shouldAlert=true म्हणजे admin ला message पाठवा.
func RecordDownload(userID primitive.ObjectID, usedToday int) (bool, int, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Existing guard record मिळव
	var g UserGuard
	err := colUserGuard().FindOne(ctx, bson.M{"user_id": userID}).Decode(&g)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return false, 0, err
	}

	score := g.SuspicionScore
	consecutive := g.ConsecutiveFullDays
	today := todayKeyIST()

	// ── Score calculation ──────────────────────────────────────────────
	added := 0

	// 5/5 downloads today
	if usedToday >= offlineDailyMax {
		added += 3

		// Consecutive full days check
		if g.LastFullDay == yesterday() {
			consecutive++
		} else if g.LastFullDay != today {
			consecutive = 1
		}

		// 3+ consecutive full days = very suspicious
		if consecutive >= 3 {
			added += 10
		}
	}

	// Odd hours (2am–5am IST) download
	hour := time.Now().In(ist).Hour()
	if hour >= 2 && hour < 5 {
		added += 2
	}

	score += added

	// Alert फक्त एकदाच — जर आधी 24h आत alert गेला असेल तर पाठवू नको
	shouldAlert := false
	if score >= SuspicionThreshold {
		if g.LastAlertedAt.IsZero() || time.Since(g.LastAlertedAt) > 24*time.Hour {
			shouldAlert = true
		}
	}

	// Update record
	update := bson.M{
		"$set": bson.M{
			"suspicion_score":       score,
			"consecutive_full_days": consecutive,
			"updated_at":            time.Now(),
		},
		"$setOnInsert": bson.M{"_id": primitive.NewObjectID()},
	}
	if g.LastFullDay != today && usedToday >= offlineDailyMax {
		update["$set"].(bson.M)["last_full_day"] = today
	}
	if shouldAlert {
		update["$set"].(bson.M)["last_alerted_at"] = time.Now()
	}

	_, err = colUserGuard().UpdateOne(ctx,
		bson.M{"user_id": userID},
		update,
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return false, score, err
	}

	return shouldAlert, score, nil
}

// GetSuspiciousUsers — admin /suspicious command साठी.
func GetSuspiciousUsers() ([]UserGuard, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := colUserGuard().Find(ctx,
		bson.M{"suspicion_score": bson.M{"$gte": SuspicionThreshold}},
		options.Find().SetSort(bson.M{"suspicion_score": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []UserGuard
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetGuard — एका user चा guard record.
func GetGuard(userID primitive.ObjectID) (*UserGuard, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var g UserGuard
	err := colUserGuard().FindOne(ctx, bson.M{"user_id": userID}).Decode(&g)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &g, nil
}

// yesterday — IST मध्ये कालचा date key.
func yesterday() string {
	return time.Now().In(ist).AddDate(0, 0, -1).Format("2006-01-02")
}
