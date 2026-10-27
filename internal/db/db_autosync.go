// internal/db/db_autosync.go
//
// AutoSync — source group/channel madhun destination channel kade scheduled copy.
//
//   autosync_tasks  : task config + source cursor (last_msg_id) + stats
//   autosync_map    : (task, source msg) -> destination msg   [DUPLICATE guard, unique index]
//   autosync_topics : (task, source topic) -> destination topic
package db

import (
	"sync"
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
	IdemOnly     bool               `bson:"-"` // backfill: fakt idempotent rules (New madhe Old nasleli)
	Resume       bool               `bson:"resume"` // run adhura (restart/timeout/cap) — scheduler aapoaap continue karel
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

// AutoSyncMapPutPlaceholder — copy fail zalyane "fake" media pathvla; asli media pudhchya run madhe replace hoil.
func AutoSyncMapPutPlaceholder(taskID primitive.ObjectID, srcMsg, destMsg int) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colAutoSyncMap().UpdateOne(ctx,
		bson.M{"task_id": taskID, "src_msg_id": srcMsg},
		bson.M{"$set": bson.M{"dest_msg_id": destMsg, "placeholder": true, "ph_tries": 0, "created_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// AutoSyncPh — placeholder entry.
type AutoSyncPh struct {
	SrcMsg  int `bson:"src_msg_id"`
	DestMsg int `bson:"dest_msg_id"`
	Tries   int `bson:"ph_tries"`
}

// AutoSyncPlaceholderList — replace baki asleli placeholders (src msg id kramane), tries < maxTries.
func AutoSyncPlaceholderList(taskID primitive.ObjectID, maxTries, limit int) []AutoSyncPh {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colAutoSyncMap().Find(ctx,
		bson.M{"task_id": taskID, "placeholder": true, "dest_msg_id": bson.M{"$gt": 0}, "ph_tries": bson.M{"$lt": maxTries}},
		options.Find().SetSort(bson.D{{Key: "src_msg_id", Value: 1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []AutoSyncPh
	_ = cur.All(ctx, &out)
	return out
}

// AutoSyncPlaceholderDone — asli media replace zali (kiva source message nahi) — placeholder flag kadhto.
func AutoSyncPlaceholderDone(taskID primitive.ObjectID, srcMsg int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncMap().UpdateOne(ctx,
		bson.M{"task_id": taskID, "src_msg_id": srcMsg},
		bson.M{"$set": bson.M{"placeholder": false}},
	)
}

// AutoSyncPlaceholderTry — replace attempt fail zala; tries vadhvto.
func AutoSyncPlaceholderTry(taskID primitive.ObjectID, srcMsg int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncMap().UpdateOne(ctx,
		bson.M{"task_id": taskID, "src_msg_id": srcMsg},
		bson.M{"$inc": bson.M{"ph_tries": 1}},
	)
}

// AutoSyncPlaceholderCount — kiti placeholders ajun replace vhaychya baki ahet.
func AutoSyncPlaceholderCount(taskID primitive.ObjectID) int {
	ctx, cancel := Ctx()
	defer cancel()
	n, _ := colAutoSyncMap().CountDocuments(ctx, bson.M{"task_id": taskID, "placeholder": true})
	return int(n)
}

// AutoSyncLinkPending — message madhe links ajun resolve zale nahit (target copy zala nahi); nantar edit karun fix.
func AutoSyncLinkPending(taskID primitive.ObjectID, srcMsg int, on bool) {
	ctx, cancel := Ctx()
	defer cancel()
	set := bson.M{"link_pending": on}
	if on {
		set["link_tries"] = 0
	}
	_, _ = colAutoSyncMap().UpdateOne(ctx, bson.M{"task_id": taskID, "src_msg_id": srcMsg}, bson.M{"$set": set})
}

type AutoSyncLp struct {
	SrcMsg  int `bson:"src_msg_id"`
	DestMsg int `bson:"dest_msg_id"`
	Tries   int `bson:"link_tries"`
}

func AutoSyncLinkPendingList(taskID primitive.ObjectID, maxTries, limit int) []AutoSyncLp {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colAutoSyncMap().Find(ctx,
		bson.M{"task_id": taskID, "link_pending": true, "dest_msg_id": bson.M{"$gt": 0}, "link_tries": bson.M{"$lt": maxTries}},
		options.Find().SetSort(bson.D{{Key: "src_msg_id", Value: 1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []AutoSyncLp
	_ = cur.All(ctx, &out)
	return out
}

func AutoSyncLinkTry(taskID primitive.ObjectID, srcMsg int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colAutoSyncMap().UpdateOne(ctx, bson.M{"task_id": taskID, "src_msg_id": srcMsg}, bson.M{"$inc": bson.M{"link_tries": 1}})
}

// AutoSyncPair — source msg -> destination msg.
type AutoSyncPair struct {
	Src  int `bson:"src_msg_id"`
	Dest int `bson:"dest_msg_id"`
}

// AutoSyncMapPage — destination msg id vadhtya kramane (afterDest nantarche) copy zalelya messages.
var autoSyncMapDestIdxOnce sync.Once

func AutoSyncMapPage(taskID primitive.ObjectID, afterDest, limit int) []AutoSyncPair {
	ctx, cancel := Ctx()
	defer cancel()
	// (task_id, dest_msg_id) index — motha map madhe sorted paging fast ani "sort memory limit" nahi.
	autoSyncMapDestIdxOnce.Do(func() {
		_, _ = colAutoSyncMap().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "task_id", Value: 1}, {Key: "dest_msg_id", Value: 1}}})
	})
	cur, err := colAutoSyncMap().Find(ctx,
		bson.M{"task_id": taskID, "dest_msg_id": bson.M{"$gt": afterDest}},
		options.Find().SetSort(bson.D{{Key: "dest_msg_id", Value: 1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []AutoSyncPair
	_ = cur.All(ctx, &out)
	return out
}

func AutoSyncMapCount(taskID primitive.ObjectID) int {
	ctx, cancel := Ctx()
	defer cancel()
	n, _ := colAutoSyncMap().CountDocuments(ctx, bson.M{"task_id": taskID, "dest_msg_id": bson.M{"$gt": 0}})
	return int(n)
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
