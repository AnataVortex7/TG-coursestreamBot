// internal/db/db_otp.go
// OTPRecord model + CRUD helpers for email OTP login.
// ColOTPs collection is declared here; it is initialised in Connect() inside mongo.go.
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

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	MaxOTPLockAttempts = 4             // 4 wrong attempts → lock
	OTPLockDuration    = 3 * time.Hour // lock duration
)

// ─── OTPRecord ────────────────────────────────────────────────────────────────

// OTPRecord stores one OTP request.
// MongoDB TTL index on expires_at auto-deletes expired records.
type OTPRecord struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	Email     string             `bson:"email"`
	OTPHash   string             `bson:"otp_hash"`   // bcrypt hash of the 4-digit code
	Attempts  int                `bson:"attempts"`   // wrong-attempt counter (max 3)
	Used      bool               `bson:"used"`       // true after successful verify
	CreatedAt time.Time          `bson:"created_at"`
	ExpiresAt time.Time          `bson:"expires_at"` // TTL — MongoDB deletes at this time
}

// ─── OTPLockout ───────────────────────────────────────────────────────────────

// OTPLockout tracks cumulative failed OTP verification attempts per email.
// Separate from OTPRecord so it persists across multiple OTP send/verify cycles.
// Without this, a user can bypass the 3-attempt limit by requesting a new OTP.
type OTPLockout struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	Email       string             `bson:"email"`
	FailCount   int                `bson:"fail_count"`
	LockedUntil time.Time          `bson:"locked_until"`
	IsLocked    bool               `bson:"is_locked"`
	UpdatedAt   time.Time          `bson:"updated_at"`
}

func colOTPLockouts() *mongo.Collection {
	return Database.Collection("otp_lockouts")
}

// ─── Lockout helpers ──────────────────────────────────────────────────────────

// CheckOTPLock — VerifyOTPHandler च्या सुरुवातीला call करा.
// locked=true असल्यास until वेळेपर्यंत OTP verification allow करू नका.
func CheckOTPLock(email string) (locked bool, until time.Time) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec OTPLockout
	err := colOTPLockouts().FindOne(ctx, bson.M{"email": email}).Decode(&rec)
	if err != nil {
		return false, time.Time{}
	}
	if !rec.IsLocked {
		return false, time.Time{}
	}
	// Auto-unblock झाला का?
	if time.Now().After(rec.LockedUntil) {
		_, _ = colOTPLockouts().UpdateOne(ctx,
			bson.M{"email": email},
			bson.M{"$set": bson.M{"is_locked": false, "fail_count": 0, "updated_at": time.Now()}},
		)
		return false, time.Time{}
	}
	return true, rec.LockedUntil
}

// RecordOTPLockFailure — wrong OTP attempt नंतर call करा.
// 4 चुकीच्या attempts नंतर email 3 तासांसाठी lock होतो.
// Returns (nowLocked bool, userMessage string)
func RecordOTPLockFailure(email string) (nowLocked bool, msg string) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec OTPLockout
	err := colOTPLockouts().FindOne(ctx, bson.M{"email": email}).Decode(&rec)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return false, ""
	}

	newCount := rec.FailCount + 1
	isLocked := newCount >= MaxOTPLockAttempts
	lockedUntil := time.Time{}
	if isLocked {
		ist := time.FixedZone("IST", 5*60*60+30*60)
        lockedUntil = time.Now().In(ist).Add(OTPLockDuration)
	}

	update := bson.M{
		"$set": bson.M{
			"fail_count":   newCount,
			"is_locked":    isLocked,
			"locked_until": lockedUntil,
			"updated_at":   time.Now(),
		},
		"$setOnInsert": bson.M{
			"_id":   primitive.NewObjectID(),
			"email": email,
		},
	}
	_, _ = colOTPLockouts().UpdateOne(ctx,
		bson.M{"email": email},
		update,
		options.Update().SetUpsert(true),
	)

	if isLocked {
		return true, fmt.Sprintf(
			"Too many wrong OTP attempts. Try again after %s.",
			lockedUntil.Format("3:04 PM, 02 Jan"),
		)
	}
	left := MaxOTPLockAttempts - newCount
	return false, fmt.Sprintf("Wrong OTP. %d attempt(s) left before 3-hour lock.", left)
}

// ResetOTPLock — successful OTP verify नंतर call करा.
func ResetOTPLock(email string) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colOTPLockouts().DeleteOne(ctx, bson.M{"email": email})
}

// ─── OTPRecord queries ────────────────────────────────────────────────────────

// CreateOTP replaces any previous OTP for email and inserts a new one.
// hashedOTP must be a bcrypt hash of the plain 4-digit code.
func CreateOTP(email, hashedOTP string) error {
	ctx, cancel := Ctx()
	defer cancel()

	// Delete all previous OTPs for this email (clean slate)
	_, _ = ColOTPs.DeleteMany(ctx, bson.M{"email": email})

	now := time.Now()
	rec := OTPRecord{
		ID:        primitive.NewObjectID(),
		Email:     email,
		OTPHash:   hashedOTP,
		Attempts:  0,
		Used:      false,
		CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	_, err := ColOTPs.InsertOne(ctx, rec)
	return err
}

// GetValidOTP returns the active (not used, not expired) OTP for email.
// Returns (nil, nil) when not found.
func GetValidOTP(email string) (*OTPRecord, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec OTPRecord
	err := ColOTPs.FindOne(ctx, bson.M{
		"email":      email,
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

// IncrementOTPAttempts adds 1 to the wrong-attempt counter for an OTP record.
func IncrementOTPAttempts(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColOTPs.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$inc": bson.M{"attempts": 1}},
	)
	return err
}

// MarkOTPUsed flags an OTP as consumed so it cannot be reused.
func MarkOTPUsed(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColOTPs.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"used": true}},
	)
	return err
}

// DeleteOTP removes all OTP records for email.
// Called when email delivery fails so the rate-limit timer resets
// and the user can retry immediately without waiting 60 seconds.
func DeleteOTP(email string) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColOTPs.DeleteMany(ctx, bson.M{"email": email})
	return err
}

// CanSendOTP returns true when at least 60 seconds have passed since
// the last OTP was issued for email (rate-limit guard).
func CanSendOTP(email string) bool {
	ctx, cancel := Ctx()
	defer cancel()

	var rec OTPRecord
	err := ColOTPs.FindOne(ctx,
		bson.M{"email": email},
		options.FindOne().SetSort(bson.M{"created_at": -1}),
	).Decode(&rec)
	if err != nil {
		// No previous OTP — allow
		return true
	}
	return time.Since(rec.CreatedAt) > 60*time.Second
}
