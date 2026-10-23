// internal/db/db_msgmap.go
//
// Userbot download (send channel) ani course delivery sathi message-id maps.
//
//  1. userbotdl_msgmap    : source channel msgID  -> send channel msgID
//     (summary msg madhle deep links send channel che navin msg id vaprun rewrite karayla)
//  2. userbotdl_summaries : send channel madhle "summary" (links asleli text) msgs
//  3. lecture_msg_map     : user + course + lecture(msgID) -> user chya chat madhla bot cha msgID
//     (course user la detana summary madhle links bot chya msg id ne rewrite karayla)
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func colUserbotMsgMap() *mongo.Collection { return Database.Collection("userbotdl_msgmap") }
func colUserbotSummaries() *mongo.Collection {
	return Database.Collection("userbotdl_summaries")
}
func colLectureMsgMap() *mongo.Collection { return Database.Collection("lecture_msg_map") }

// ── 1. source msg -> send channel msg ────────────────────────────────────────

// SaveUserbotMsgMap — srcChat/destChat he RAW (positive, -100 shivay) channel ID.
func SaveUserbotMsgMap(srcChat int64, srcMsg int, destChat int64, destMsg int) error {
	if srcMsg <= 0 || destMsg <= 0 {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colUserbotMsgMap().UpdateOne(ctx,
		bson.M{"src_chat_id": srcChat, "src_msg_id": srcMsg, "dest_chat_id": destChat},
		bson.M{"$set": bson.M{"dest_msg_id": destMsg, "updated_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// GetUserbotMsgMap — dilelya srcMsgs sathi {srcMsgID: destMsgID} map.
func GetUserbotMsgMap(srcChat, destChat int64, srcMsgs []int) (map[int]int, error) {
	out := make(map[int]int, len(srcMsgs))
	if len(srcMsgs) == 0 {
		return out, nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colUserbotMsgMap().Find(ctx, bson.M{
		"src_chat_id":  srcChat,
		"dest_chat_id": destChat,
		"src_msg_id":   bson.M{"$in": srcMsgs},
	})
	if err != nil {
		return out, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var d struct {
			SrcMsgID  int `bson:"src_msg_id"`
			DestMsgID int `bson:"dest_msg_id"`
		}
		if cur.Decode(&d) == nil {
			out[d.SrcMsgID] = d.DestMsgID
		}
	}
	return out, cur.Err()
}

// ── 2. summary msgs ──────────────────────────────────────────────────────────

type UserbotSummary struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	SrcChatID  int64              `bson:"src_chat_id"`
	SrcMsgID   int                `bson:"src_msg_id"`
	DestChatID int64              `bson:"dest_chat_id"`
	DestMsgID  int                `bson:"dest_msg_id"`
	Unresolved int                `bson:"unresolved"` // kiti links cha target ajun copy zalela navhta
	CreatedAt  time.Time          `bson:"created_at"`
}

func SaveUserbotSummary(srcChat int64, srcMsg int, destChat int64, destMsg int, unresolved int) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colUserbotSummaries().UpdateOne(ctx,
		bson.M{"src_chat_id": srcChat, "src_msg_id": srcMsg, "dest_chat_id": destChat},
		bson.M{
			"$set":         bson.M{"dest_msg_id": destMsg, "unresolved": unresolved},
			"$setOnInsert": bson.M{"created_at": time.Now()},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

// ListUserbotSummaries — ya send channel (destChat) madhle sagle summary msgs (jaste kadun kami id).
func ListUserbotSummaries(destChat int64) ([]UserbotSummary, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colUserbotSummaries().Find(ctx, bson.M{"dest_chat_id": destChat},
		options.Find().SetSort(bson.M{"dest_msg_id": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var list []UserbotSummary
	if err = cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ListUnresolvedSummaries — jyanche links cha target copy navhte zale (retry nantar fix karayche).
func ListUnresolvedSummaries(srcChat, destChat int64) ([]UserbotSummary, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colUserbotSummaries().Find(ctx, bson.M{
		"src_chat_id": srcChat, "dest_chat_id": destChat, "unresolved": bson.M{"$gt": 0},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var list []UserbotSummary
	if err = cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ── 3. user + course + lecture -> bot cha msg (user chya chat madhla) ────────

func RecordLectureMsgMap(userID, courseID primitive.ObjectID, lectureMsgID int, chatID, botID int64, sentMsgID int) error {
	if lectureMsgID <= 0 || sentMsgID <= 0 {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colLectureMsgMap().UpdateOne(ctx,
		bson.M{"user_id": userID, "course_id": courseID, "lecture_msg_id": lectureMsgID, "chat_id": chatID, "bot_id": botID},
		bson.M{"$set": bson.M{"sent_msg_id": sentMsgID, "updated_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// GetLectureMsgMap — {lectureMsgID: botSentMsgID} (user chya chat madhla).
func GetLectureMsgMap(userID, courseID primitive.ObjectID, chatID, botID int64) (map[int]int, error) {
	out := map[int]int{}
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colLectureMsgMap().Find(ctx, bson.M{
		"user_id": userID, "course_id": courseID, "chat_id": chatID, "bot_id": botID,
	})
	if err != nil {
		return out, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var d struct {
			LectureMsgID int `bson:"lecture_msg_id"`
			SentMsgID    int `bson:"sent_msg_id"`
		}
		if cur.Decode(&d) == nil {
			out[d.LectureMsgID] = d.SentMsgID
		}
	}
	return out, cur.Err()
}
