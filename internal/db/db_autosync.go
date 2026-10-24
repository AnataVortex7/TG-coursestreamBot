// internal/db/db_autosync.go
//
// AutoSync — source group/channel madhun destination channel kade scheduled copy.
//
//   autosync_tasks  : task config + source cursor (last_msg_id) + stats
//   autosync_map    : (task, source msg) -> destination msg   [DUPLICATE guard, unique index]
//   autosync_topics : (task, source topic) -> destination topic
package db

import (
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type AutoSyncTask struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"`
	Name         string             `bson:"name"`
	Enabled      bool               `bson:"enabled"`
	SessionKey   string             `bson:"session_key"` // userbot_session doc key ("main" / "user_<id>")
	OwnerID      int64              `bson:"owner_id"`    // task banvnara admin
	SourceChat   int64              `bson:"source_chat"` // Bot-API style (-100xxxxxxxxxx)
	SourceTitle  string             `bson:"source_title"`
	DestChat     int64              `bson:"dest_chat"`
	DestTitle    string             `bson:"dest_title"`
	MirrorTopics bool               `bson:"mirror_topics"`
	Mode         string             `bson:"mode"`  // "fast" | "full"
	Times        []string           `bson:"times"` // "HH:MM" (IST)
	Replaces     []ReplaceRule      `bson:"replaces"`
	LastMsgID    int                `bson:"last_msg_id"` // source cursor — tyapudhche msgs navin
	LastRunAt    time.Time          `bson:"last_run_at"`
	LastStatus   string             `bson:"last_status"`
	TotalCopied  int64              `bson:"total_copied"`
	CreatedAt    time.Time          `bson:"created_at"`
}

func colAutoSyncTasks() *mongo.Collection  { return Database.Collection("autosync_tasks") }
func colAutoSyncMap() *mongo.Collection    { return Database.Collection("autosync_map") }
func colAutoSyncTopics() *mongo.Collection { return Database.Collection("autosync_topics") }
func colAutoSyncFails() *mongo.Collection  { return Database.Collection("autosync_fails") }

// EnsureAutoSyncIndexes — unique indexes (duplicate protection DB level var pan).
func EnsureAutoSyncIndexes() {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncMap().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "task_id", Value: 1}, {Key: "src_msg_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	_, _ = colAutoSyncTopics().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "task_id", Value: 1}, {Key: "src_topic", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
}

func CreateAutoSyncTask(t *AutoSyncTask) error {
	ctx, cancel := Ctx()
	defer cancel()
	t.ID = primitive.NewObjectID()
	t.CreatedAt = time.Now()
	_, err := colAutoSyncTasks().InsertOne(ctx, t)
	return err
}

func ListAutoSyncTasks() ([]AutoSyncTask, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colAutoSyncTasks().Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []AutoSyncTask
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func GetAutoSyncTask(id primitive.ObjectID) (*AutoSyncTask, error) {
	ctx, cancel := Ctx()
	defer cancel()
	var t AutoSyncTask
	err := colAutoSyncTasks().FindOne(ctx, bson.M{"_id": id}).Decode(&t)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func SetAutoSyncFields(id primitive.ObjectID, fields bson.M) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colAutoSyncTasks().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields})
	return err
}

func IncAutoSyncCopied(id primitive.ObjectID, n int64) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colAutoSyncTasks().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$inc": bson.M{"total_copied": n}})
	return err
}

func DeleteAutoSyncTask(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	if _, err := colAutoSyncTasks().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		return err
	}
	_, _ = colAutoSyncMap().DeleteMany(ctx, bson.M{"task_id": id})
	_, _ = colAutoSyncTopics().DeleteMany(ctx, bson.M{"task_id": id})
	_, _ = colAutoSyncFails().DeleteMany(ctx, bson.M{"task_id": id})
	return nil
}

// AutoSyncFailInc — ya message cha send kiti vela (runs madhe) fail zala; navin count return karto.
func AutoSyncFailInc(taskID primitive.ObjectID, srcMsg int) int {
	ctx, cancel := Ctx()
	defer cancel()
	var d struct {
		Count int `bson:"count"`
	}
	err := colAutoSyncFails().FindOneAndUpdate(ctx,
		bson.M{"task_id": taskID, "src_msg_id": srcMsg},
		bson.M{"$inc": bson.M{"count": 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&d)
	if err != nil {
		return 1
	}
	return d.Count
}

// AutoSyncTopicDelete — destination topic delete zala asel tar mapping kadhto (parat banvla jail).
func AutoSyncTopicDelete(taskID primitive.ObjectID, srcTopic int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncTopics().DeleteOne(ctx, bson.M{"task_id": taskID, "src_topic": srcTopic})
}

// AutoSyncMapGet — source msg already copy zala ka? (destMsgID 0 = skip kelela)
func AutoSyncMapGet(taskID primitive.ObjectID, srcMsg int) (int, bool) {
	ctx, cancel := Ctx()
	defer cancel()
	var d struct {
		DestMsgID int `bson:"dest_msg_id"`
	}
	err := colAutoSyncMap().FindOne(ctx, bson.M{"task_id": taskID, "src_msg_id": srcMsg}).Decode(&d)
	if err != nil {
		return 0, false
	}
	return d.DestMsgID, true
}

func AutoSyncMapPut(taskID primitive.ObjectID, srcMsg, destMsg int) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colAutoSyncMap().UpdateOne(ctx,
		bson.M{"task_id": taskID, "src_msg_id": srcMsg},
		bson.M{"$set": bson.M{"dest_msg_id": destMsg, "created_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// AutoSyncMapDeleteFrom — reset sathi: src_msg_id > afterID che map entries kadhto (parat copy hotil).
func AutoSyncMapDeleteFrom(taskID primitive.ObjectID, afterID int) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncFails().DeleteMany(ctx, bson.M{"task_id": taskID, "src_msg_id": bson.M{"$gt": afterID}})
	_, err := colAutoSyncMap().DeleteMany(ctx, bson.M{"task_id": taskID, "src_msg_id": bson.M{"$gt": afterID}})
	return err
}

func AutoSyncTopicGet(taskID primitive.ObjectID, srcTopic int) (int, bool) {
	ctx, cancel := Ctx()
	defer cancel()
	var d struct {
		DestTopic int `bson:"dest_topic"`
	}
	err := colAutoSyncTopics().FindOne(ctx, bson.M{"task_id": taskID, "src_topic": srcTopic}).Decode(&d)
	if err != nil || d.DestTopic <= 0 {
		return 0, false
	}
	return d.DestTopic, true
}

func AutoSyncTopicPut(taskID primitive.ObjectID, srcTopic, destTopic int) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colAutoSyncTopics().UpdateOne(ctx,
		bson.M{"task_id": taskID, "src_topic": srcTopic},
		bson.M{"$set": bson.M{"dest_topic": destTopic, "created_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}
