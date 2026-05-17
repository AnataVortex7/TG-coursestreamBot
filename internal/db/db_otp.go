// internal/db/otp.go
// OTPRecord model + CRUD helpers for email OTP login.
// ColOTPs collection is declared here; it is initialised in Connect() inside mongo.go.
package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Model ────────────────────────────────────────────────────────────────────

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

// ─── Queries ─────────────────────────────────────────────────────────────────

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
