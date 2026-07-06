// internal/db/db_login_guard.go
// Login attempt tracking — 4 wrong passwords = 5 hour account block.
package db

import (
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	MaxLoginAttempts = 4
	LoginBlockFor    = 5 * time.Hour
)

// LoginAttempt — per-identifier failed login tracking.
type LoginAttempt struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"`
	Identifier   string             `bson:"identifier"` // email or mobile
	Attempts     int                `bson:"attempts"`
	BlockedUntil time.Time          `bson:"blocked_until"`
	IsBlocked    bool               `bson:"is_blocked"`
	UpdatedAt    time.Time          `bson:"updated_at"`
}

func colLoginAttempts() *mongo.Collection {
	return Database.Collection("login_attempts")
}

// CheckLoginBlock — login handler च्या सुरुवातीला call करा.
// blocked=true असल्यास until वेळेपर्यंत login allow करू नका.
func CheckLoginBlock(identifier string) (blocked bool, until time.Time) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec LoginAttempt
	err := colLoginAttempts().FindOne(ctx, bson.M{"identifier": identifier}).Decode(&rec)
	if err != nil {
		return false, time.Time{}
	}
	if !rec.IsBlocked {
		return false, time.Time{}
	}
	// Auto-unblock झाला का?
	if time.Now().After(rec.BlockedUntil) {
		_, _ = colLoginAttempts().UpdateOne(ctx,
			bson.M{"identifier": identifier},
			bson.M{"$set": bson.M{"is_blocked": false, "attempts": 0, "updated_at": time.Now()}},
		)
		return false, time.Time{}
	}
	return true, rec.BlockedUntil
}

// RecordFailedLogin — wrong password नंतर call करा.
// Returns (nowBlocked bool, message string)
func RecordFailedLogin(identifier string) (nowBlocked bool, msg string) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec LoginAttempt
	err := colLoginAttempts().FindOne(ctx, bson.M{"identifier": identifier}).Decode(&rec)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return false, "Invalid credentials. Please try again."
	}

	newAttempts := rec.Attempts + 1
	isBlocked := newAttempts >= MaxLoginAttempts
	blockedUntil := time.Time{}
	if isBlocked {
		blockedUntil = time.Now().Add(LoginBlockFor)
	}

	update := bson.M{
		"$set": bson.M{
			"attempts":      newAttempts,
			"is_blocked":    isBlocked,
			"blocked_until": blockedUntil,
			"updated_at":    time.Now(),
		},
		"$setOnInsert": bson.M{
			"_id":        primitive.NewObjectID(),
			"identifier": identifier,
		},
	}
	_, _ = colLoginAttempts().UpdateOne(ctx,
		bson.M{"identifier": identifier},
		update,
		options.Update().SetUpsert(true),
	)

	if isBlocked {
		return true, "Too many wrong attempts. Account blocked for 5 hours."
	}
	left := MaxLoginAttempts - newAttempts
	return false, fmt.Sprintf("Invalid credentials. %d attempt(s) left before account lock.", left)
}

// ResetLoginAttempts — successful login नंतर call करा.
func ResetLoginAttempts(identifier string) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colLoginAttempts().DeleteOne(ctx, bson.M{"identifier": identifier})
}
