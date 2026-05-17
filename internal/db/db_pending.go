// internal/db/db_pending.go
// Temporary store for unverified registrations.
// A PendingRegistration holds the validated form data + OTP hash.
// MongoDB TTL index deletes records after 10 minutes automatically.
package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// ─── Model ────────────────────────────────────────────────────────────────────

type PendingRegistration struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"`
	Email        string             `bson:"email"`
	Mobile       string             `bson:"mobile"`
	FullName     string             `bson:"full_name"`
	PasswordHash string             `bson:"password_hash"`
	OTPHash      string             `bson:"otp_hash"`
	Attempts     int                `bson:"attempts"`
	CreatedAt    time.Time          `bson:"created_at"`
	ExpiresAt    time.Time          `bson:"expires_at"` // TTL — MongoDB auto-deletes
}

// ─── Queries ─────────────────────────────────────────────────────────────────

// CreatePendingRegistration stores a new pending registration.
// Any previous pending record for the same email is replaced.
func CreatePendingRegistration(email, mobile, fullName, passwordHash, otpHash string) error {
	ctx, cancel := Ctx()
	defer cancel()

	// Clean up any previous pending record for this email
	_, _ = ColPendingRegs.DeleteMany(ctx, bson.M{"email": email})

	now := time.Now()
	rec := PendingRegistration{
		ID:           primitive.NewObjectID(),
		Email:        email,
		Mobile:       mobile,
		FullName:     fullName,
		PasswordHash: passwordHash,
		OTPHash:      otpHash,
		Attempts:     0,
		CreatedAt:    now,
		ExpiresAt:    now.Add(10 * time.Minute),
	}
	_, err := ColPendingRegs.InsertOne(ctx, rec)
	return err
}

// GetPendingRegistration returns the active pending record for email.
// Returns (nil, nil) when not found or expired.
func GetPendingRegistration(email string) (*PendingRegistration, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec PendingRegistration
	err := ColPendingRegs.FindOne(ctx, bson.M{
		"email":      email,
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

// IncrementPendingAttempts adds 1 to the wrong-attempt counter.
func IncrementPendingAttempts(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColPendingRegs.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$inc": bson.M{"attempts": 1}},
	)
	return err
}

// DeletePendingRegistration removes the pending record for email.
// Called after successful OTP verify (record no longer needed).
func DeletePendingRegistration(email string) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColPendingRegs.DeleteMany(ctx, bson.M{"email": email})
	return err
}

// CanSendRegOTP returns true when no pending record exists for email
// OR when 60 seconds have passed since the last one was created.
func CanSendRegOTP(email string) bool {
	ctx, cancel := Ctx()
	defer cancel()

	var rec PendingRegistration
	err := ColPendingRegs.FindOne(ctx, bson.M{"email": email}).Decode(&rec)
	if err != nil {
		return true // no record — allow
	}
	return time.Since(rec.CreatedAt) > 60*time.Second
}
