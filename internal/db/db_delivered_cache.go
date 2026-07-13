// internal/db/db_delivered_cache.go
//
// In-bot "Watch" flow साठी — एका user ला एकदा watermark लावून पाठवलेली
// lecture चा LOG_CHANNEL मधला copy इथे लक्षात ठेवला जातो. पुढच्या वेळी
// तोच user तीच lecture परत उघडेल तेव्हा नवीन watermark job न चालवता,
// इथून मिळालेल्या log_channel_msg_id वरून थेट (जलद) forward केला जातो.
//
// [PERF] प्रत्येक lecture-click ला DB call टाळण्यासाठी per-user in-memory
// cache (cache.go: deliveredCacheBucket) वापरतो — पहिल्यांदा DB मधून
// वाचल्यावर तीच value थोडा वेळ (userCacheTTL) memory मध्ये ठेवतो. "मिळालाच
// नाही" (nil) हा result सुद्धा cache होतो — म्हणजे नवीन lecture साठी वारंवार
// येणारा "not found yet" query पण प्रत्येक वेळी DB ला जात नाही.
package db

import (
	"errors"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func deliveredCacheKey(userID, courseID primitive.ObjectID, messageID int) string {
	return userID.Hex() + ":" + courseID.Hex() + ":" + strconv.Itoa(messageID)
}

// GetDeliveredCache — या user/course/message साठी आधीच cache केलेली copy आहे का ते बघतो.
func GetDeliveredCache(userID, courseID primitive.ObjectID, messageID int) (*DeliveredCache, error) {
	cacheKey := deliveredCacheKey(userID, courseID, messageID)
	if v, ok := deliveredCacheBucket.get(cacheKey); ok {
		rec, _ := v.(*DeliveredCache)
		return rec, nil
	}

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
			deliveredCacheBucket.set(cacheKey, (*DeliveredCache)(nil), userCacheTTL)
			return nil, nil
		}
		return nil, err
	}
	deliveredCacheBucket.set(cacheKey, &rec, userCacheTTL)
	return &rec, nil
}

// UpsertDeliveredCache — नवीन watermarked/plain copy पाठवल्यावर तिचा log-channel
// reference save करतो (पुढच्या वेळी fast-path साठी). DB लिहितानाच local
// cache पण लगेच update करतो — त्याच session मध्ये लगेच पुढचा request असेल
// तर DB ला परत विचारावं लागणार नाही.
func UpsertDeliveredCache(userID, courseID primitive.ObjectID, messageID, logChannelMsgID int, watermarked bool) error {
	ctx, cancel := Ctx()
	defer cancel()

	now := time.Now()
	_, err := ColDeliveredCache.UpdateOne(ctx,
		bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID},
		bson.M{"$set": bson.M{
			"log_channel_msg_id": logChannelMsgID,
			"watermarked":        watermarked,
			"created_at":         now,
		}},
		options.Update().SetUpsert(true),
	)
	if err == nil {
		cacheKey := deliveredCacheKey(userID, courseID, messageID)
		deliveredCacheBucket.set(cacheKey, &DeliveredCache{
			UserID: userID, CourseID: courseID, MessageID: messageID,
			LogChannelMsgID: logChannelMsgID, Watermarked: watermarked, CreatedAt: now,
		}, userCacheTTL)
	}
	return err
}

// GetDeliveredMessageIDs — या user ने या course मध्ये आत्तापर्यंत कुठले
// lectures (original channel message_id) मागवले आहेत ते सगळे एकत्र देतो —
// folder-listing मध्ये "✅ आधीच पाहिलं/डाउनलोड केलं" mark दाखवायला वापरतो
// (एका DB call मध्ये संपूर्ण course साठी, प्रत्येक lecture साठी वेगळा call टाळून).
func GetDeliveredMessageIDs(userID, courseID primitive.ObjectID) (map[int]bool, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColDeliveredCache.Find(ctx,
		bson.M{"user_id": userID, "course_id": courseID},
		options.Find().SetProjection(bson.M{"message_id": 1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	result := make(map[int]bool)
	var rows []struct {
		MessageID int `bson:"message_id"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		result[r.MessageID] = true
	}
	return result, nil
}

// DeleteDeliveredCacheForCourse — course delete/re-upload झाल्यावर जुने cache references साफ करतो.
// (Per-user in-memory cache इथे साफ करत नाही — ती TTL नुसार आपोआप expire होईल;
// course delete/re-upload क्वचित घडतं म्हणून हे स्वीकारार्ह आहे.)
func DeleteDeliveredCacheForCourse(courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColDeliveredCache.DeleteMany(ctx, bson.M{"course_id": courseID})
	return err
}
