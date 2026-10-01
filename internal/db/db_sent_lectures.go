// internal/db/db_sent_lectures.go
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func colSentLectureMsgs() *mongo.Collection { return Database.Collection("sent_lecture_msgs") }

// RecordSentLectureMsg — video delivery पूर्ण झाल्यावर call करा.
// botID: कोणत्या bot ने पाठवला (client.Self.ID). Legacy साठी 0 acceptable.
func RecordSentLectureMsg(userID, courseID primitive.ObjectID, chatID int64, messageID int, botID int64) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colSentLectureMsgs().InsertOne(ctx, SentLectureMsg{
		UserID:    userID,
		CourseID:  courseID,
		ChatID:    chatID,
		MessageID: messageID,
		BotID:     botID,
		CreatedAt: time.Now(),
	})
	return err
}

// GetSentLectureMsgsByBot — या machine च्या bot ने पाठवलेले records.
// botID = 0 असेल तर legacy records (bot_id missing) पण include करतो.
func GetSentLectureMsgsByBot(userID, courseID primitive.ObjectID, botID int64) ([]SentLectureMsg, error) {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{
		"user_id":   userID,
		"course_id": courseID,
		"$or": bson.A{
			bson.M{"bot_id": botID},
			bson.M{"bot_id": bson.M{"$exists": false}},
			bson.M{"bot_id": 0},
		},
	}
	cursor, err := colSentLectureMsgs().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []SentLectureMsg
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetSentLectureMsgsForAccess — सगळ्या bots चे records (legacy support).
func GetSentLectureMsgsForAccess(userID, courseID primitive.ObjectID) ([]SentLectureMsg, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := colSentLectureMsgs().Find(ctx, bson.M{"user_id": userID, "course_id": courseID})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []SentLectureMsg
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteSentLectureMsgsByIDs — specific message IDs चे records delete करतो.
// फक्त successfully deleted messages साठी call करा — failed ते DB मध्ये
// राहतात आणि पुढच्या cleanup run मध्ये retry होतात.
func DeleteSentLectureMsgsByIDs(messageIDs []int) error {
	if len(messageIDs) == 0 {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colSentLectureMsgs().DeleteMany(ctx, bson.M{
		"message_id": bson.M{"$in": messageIDs},
	})
	return err
}

// DeleteSentLectureMsgsByBot — या bot ने पाठवलेले records delete करतो.
// Legacy (bot_id=0 / missing) records पण या bot ने delete केले असे समजतो.
func DeleteSentLectureMsgsByBot(userID, courseID primitive.ObjectID, botID int64) error {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{
		"user_id":   userID,
		"course_id": courseID,
		"$or": bson.A{
			bson.M{"bot_id": botID},
			bson.M{"bot_id": bson.M{"$exists": false}},
			bson.M{"bot_id": 0},
		},
	}
	_, err := colSentLectureMsgs().DeleteMany(ctx, filter)
	return err
}

// HasPendingSentMsgs — या access साठी अजून कुठल्याही bot चे pending records आहेत का.
// false = सगळ्या bots नी delete केलं → access ला cleaned mark करता येतो.
func HasPendingSentMsgs(userID, courseID primitive.ObjectID) (bool, error) {
	ctx, cancel := Ctx()
	defer cancel()

	count, err := colSentLectureMsgs().CountDocuments(ctx, bson.M{
		"user_id":   userID,
		"course_id": courseID,
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// DeleteSentLectureMsgsForAccess — सगळे records delete (legacy / single-bot use).
func DeleteSentLectureMsgsForAccess(userID, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colSentLectureMsgs().DeleteMany(ctx, bson.M{"user_id": userID, "course_id": courseID})
	return err
}
