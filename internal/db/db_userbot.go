// internal/db/db_userbot.go
//
// UserBot session MongoDB madhe save / load / delete karto.
// Ek collection "userbot_session" — always single document (upsert).

package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const userbotSessionKey = "main"

// UserBotSessionDoc — MongoDB document
type UserBotSessionDoc struct {
	Key       string    `bson:"key"`
	Session   string    `bson:"session"`
	Phone     string    `bson:"phone"`
	FirstName string    `bson:"first_name"`
	LastName  string    `bson:"last_name"`
	Username  string    `bson:"username"`
	UserID    int64     `bson:"user_id"`
	SavedAt   time.Time `bson:"saved_at"`
}

func colUserbot() *mongo.Collection {
	return Database.Collection("userbot_session")
}

// SaveUserBotSession — session string ani user info MongoDB madhe save karto.
func SaveUserBotSession(sessionStr, phone, firstName, lastName, username string, userID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	doc := bson.M{
		"key":        userbotSessionKey,
		"session":    sessionStr,
		"phone":      phone,
		"first_name": firstName,
		"last_name":  lastName,
		"username":   username,
		"user_id":    userID,
		"saved_at":   time.Now(),
	}
	filter := bson.M{"key": userbotSessionKey}
	opts := options.Replace().SetUpsert(true)
	_, err := colUserbot().ReplaceOne(ctx, filter, doc, opts)
	return err
}

// GetUserBotSession — session string return karto (empty = nahi).
func GetUserBotSession() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var doc UserBotSessionDoc
	err := colUserbot().FindOne(ctx, bson.M{"key": userbotSessionKey}).Decode(&doc)
	if err != nil {
		return "", nil
	}
	return doc.Session, nil
}

// GetUserBotSessionDoc — full document (status display saathi).
func GetUserBotSessionDoc() (*UserBotSessionDoc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var doc UserBotSessionDoc
	err := colUserbot().FindOne(ctx, bson.M{"key": userbotSessionKey}).Decode(&doc)
	if err != nil {
		return nil, nil
	}
	return &doc, nil
}

// DeleteUserBotSession — logout kelavar session delete karto.
func DeleteUserBotSession() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := colUserbot().DeleteOne(ctx, bson.M{"key": userbotSessionKey})
	return err
}
