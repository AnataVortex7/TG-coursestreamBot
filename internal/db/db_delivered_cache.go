// internal/db/db_delivered_cache.go
//
// In-bot "Watch" flow साठी — एका user ला एकदा watermark लावून पाठवलेली
// lecture चा LOG_CHANNEL मधला copy इथे लक्षात ठेवला जातो. पुढच्या वेळी
// तोच user तीच lecture परत उघडेल तेव्हा नवीन watermark job न चालवता,
// इथून मिळालेल्या log_channel_msg_id वरून थेट (जलद) forward केला जातो.
package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// GetDeliveredCache — या user/course/message साठी आधीच cache केलेली copy आहे का ते बघतो.
func GetDeliveredCache(userID, courseID primitive.ObjectID, messageID int) (*DeliveredCache, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec DeliveredCache
	err := ColDeliveredCache.FindOne(ctx, bson.M{
		"user_id":    userID,
		"course_id":  courseID,
		"message_id": messageID,
	}).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// UpsertDeliveredCache — नवीन watermarked/plain copy पाठवल्यावर तिचा log-channel
// reference save करतो (पुढच्या वेळी fast-path साठी).
func UpsertDeliveredCache(userID, courseID primitive.ObjectID, messageID, logChannelMsgID int, watermarked bool) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColDeliveredCache.UpdateOne(ctx,
		bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID},
		bson.M{"$set": bson.M{
			"log_channel_msg_id": logChannelMsgID,
			"watermarked":        watermarked,
			"created_at":         time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}

// DeleteDeliveredCacheForCourse — course delete/re-upload झाल्यावर जुने cache references साफ करतो.
func DeleteDeliveredCacheForCourse(courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColDeliveredCache.DeleteMany(ctx, bson.M{"course_id": courseID})
	return err
}
