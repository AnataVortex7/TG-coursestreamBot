// internal/db/db_pending_deletions.go
//
// pending_msg_deletions — video delivery झाल्यावर record ठेवतो.
// old machine ला हे माहीत नाही — sent_lecture_msgs corrupt झाली तरी
// हे collection safe राहतं. New machine हून retry होत राहतो.
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type PendingMsgDeletion struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"`
	ChatID    int64              `bson:"chat_id"`
	MessageID int                `bson:"message_id"`
	BotID     int64              `bson:"bot_id"`
	Attempts  int                `bson:"attempts"`
	LastTried time.Time          `bson:"last_tried,omitempty"`
	CreatedAt time.Time          `bson:"created_at"`
}

func colPendingDeletions() *mongo.Collection {
	return Database.Collection("pending_msg_deletions")
}

// AddPendingDeletion — video पाठवल्यावर लगेच call करा.
func AddPendingDeletion(userID, courseID primitive.ObjectID, chatID int64, messageID int, botID int64) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingDeletions().InsertOne(ctx, PendingMsgDeletion{
		UserID:    userID,
		CourseID:  courseID,
		ChatID:    chatID,
		MessageID: messageID,
		BotID:     botID,
		CreatedAt: time.Now(),
	})
	return err
}

// GetPendingDeletionsForBot — या bot च्या expired accesses साठी delete pending records.
// फक्त expired/revoked accesses साठी — active access चे messages touch नाही.
func GetPendingDeletionsForBot(botID int64) ([]PendingMsgDeletion, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Only fetch where access is expired or revoked (join via user+course)
	// Simple approach: fetch all for this bot, caller filters by access status
	cursor, err := colPendingDeletions().Find(ctx,
		bson.M{"bot_id": botID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []PendingMsgDeletion
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// RemovePendingDeletion — successfully deleted झाल्यावर record काढा.
func RemovePendingDeletion(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingDeletions().DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// IncrementPendingDeletionAttempt — failed attempt track करतो.
func IncrementPendingDeletionAttempt(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingDeletions().UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$inc": bson.M{"attempts": 1}, "$set": bson.M{"last_tried": time.Now()}},
	)
	return err
}

// RemovePendingDeletionsByAccess — access renew झाला तर pending records साफ करा.
func RemovePendingDeletionsByAccess(userID, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingDeletions().DeleteMany(ctx, bson.M{
		"user_id":   userID,
		"course_id": courseID,
	})
	return err
}
