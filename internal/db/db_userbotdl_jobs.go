// internal/db/db_userbotdl_jobs.go
//
// userbotdl_jobs — /userbotdl चा प्रत्येक run इथे save होतो (params + checkpoint).
// Server restart झाल्यावर status=="running" असलेले jobs आपोआप शेवटच्या checkpoint
// पासून पुढे सुरू होतात (human interaction शिवाय). User ने /stopuserbotdl केला किंवा
// job पूर्ण झाला तर status बदलतो आणि पुन्हा सुरू होत नाही.
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	UserbotDLRunning    = "running"
	UserbotDLDone       = "done"
	UserbotDLStopped    = "stopped"
	UserbotDLSuperseded = "superseded"
	UserbotDLFailed     = "failed"
)

type UserbotDLJob struct {
	ID             primitive.ObjectID `bson:"_id,omitempty"`
	SenderID       int64              `bson:"sender_id"`
	AdminChatID    int64              `bson:"admin_chat_id"`
	FirstName      string             `bson:"first_name"`
	LastName       string             `bson:"last_name"`
	ChatIDRaw      int64              `bson:"chat_id_raw"`
	ChatID         int64              `bson:"chat_id"`
	TopicID        int                `bson:"topic_id"`
	StartMsg       int                `bson:"start_msg"`
	EndMsg         int                `bson:"end_msg"`
	SpecificMsgIDs []int              `bson:"specific_msg_ids,omitempty"`
	// [FIX] Job सुरू झाला तेव्हाचा send channel — restart नंतर resume झाल्यावर तोच destination वापरायला.
	SendChannelID   int64 `bson:"send_channel_id,omitempty"`
	SendChannelHash int64 `bson:"send_channel_hash,omitempty"`
	NextIdx        int                `bson:"next_idx"` // msgIDList[:NextIdx] पूर्ण झालं
	Success        int                `bson:"success"`
	Skip           int                `bson:"skip"`
	Errors         int                `bson:"errors"`
	Resumes        int                `bson:"resumes"`
	Status         string             `bson:"status"`
	CreatedAt      time.Time          `bson:"created_at"`
	UpdatedAt      time.Time          `bson:"updated_at"`
}

func colUserbotDLJobs() *mongo.Collection { return Database.Collection("userbotdl_jobs") }

// CreateUserbotDLJob — नवीन job save करतो. त्याच sender+source chat चे जुने "running"
// jobs "superseded" करतो (म्हणजे restart वर तेच दोनदा resume होणार नाहीत).
func CreateUserbotDLJob(j *UserbotDLJob) (primitive.ObjectID, error) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colUserbotDLJobs().UpdateMany(ctx,
		bson.M{"sender_id": j.SenderID, "chat_id": j.ChatID, "status": UserbotDLRunning},
		bson.M{"$set": bson.M{"status": UserbotDLSuperseded, "updated_at": time.Now()}})
	j.Status = UserbotDLRunning
	j.CreatedAt = time.Now()
	j.UpdatedAt = j.CreatedAt
	res, err := colUserbotDLJobs().InsertOne(ctx, j)
	if err != nil {
		return primitive.NilObjectID, err
	}
	id, _ := res.InsertedID.(primitive.ObjectID)
	return id, nil
}

// CheckpointUserbotDLJob — "nextIdx च्या आधीचे सगळे message handle झाले".
func CheckpointUserbotDLJob(id primitive.ObjectID, nextIdx, success, skip, errs int) {
	if id.IsZero() {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colUserbotDLJobs().UpdateOne(ctx, bson.M{"_id": id, "status": UserbotDLRunning},
		bson.M{"$set": bson.M{
			"next_idx": nextIdx, "success": success, "skip": skip, "errors": errs,
			"updated_at": time.Now(),
		}})
}

func SetUserbotDLJobStatus(id primitive.ObjectID, status string) {
	if id.IsZero() {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colUserbotDLJobs().UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$set": bson.M{"status": status, "updated_at": time.Now()}})
}

func IncUserbotDLJobResumes(id primitive.ObjectID) {
	if id.IsZero() {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colUserbotDLJobs().UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$inc": bson.M{"resumes": 1}, "$set": bson.M{"updated_at": time.Now()}})
}

// ListRunningUserbotDLJobs — restart नंतर resume करायचे jobs (जुने आधी).
func ListRunningUserbotDLJobs() ([]UserbotDLJob, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colUserbotDLJobs().Find(ctx, bson.M{"status": UserbotDLRunning},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []UserbotDLJob
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
