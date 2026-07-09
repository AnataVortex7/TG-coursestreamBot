// internal/db/db_sent_lectures.go
//
// प्रत्येक वेळी bot ने student च्या chat मध्ये lecture video पाठवला की एक
// SentLectureMsg record — access expire झाल्यावर नेमकं कुठलं message कुठल्या
// chat मधून delete करायचं ते यावरून कळतं.
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func colSentLectureMsgs() *mongo.Collection { return Database.Collection("sent_lecture_msgs") }

// RecordSentLectureMsg — video delivery पूर्ण झाल्यावर call करा (catalog.go).
func RecordSentLectureMsg(userID, courseID primitive.ObjectID, chatID int64, messageID int) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colSentLectureMsgs().InsertOne(ctx, SentLectureMsg{
		UserID:    userID,
		CourseID:  courseID,
		ChatID:    chatID,
		MessageID: messageID,
		CreatedAt: time.Now(),
	})
	return err
}

// GetSentLectureMsgsForAccess — या user+course साठी आत्तापर्यंत पाठवलेले सर्व
// lecture messages (delete करण्यासाठी).
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

// DeleteSentLectureMsgsForAccess — delete-from-chat पूर्ण झाल्यावर tracking records साफ करतो.
// (LOG_CHANNEL मधलं DeliveredCache मात्र इथे touch होत नाही — renew झाल्यावर
// तिथूनच जलद परत पाठवायचं आहे.)
func DeleteSentLectureMsgsForAccess(userID, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colSentLectureMsgs().DeleteMany(ctx, bson.M{"user_id": userID, "course_id": courseID})
	return err
}
