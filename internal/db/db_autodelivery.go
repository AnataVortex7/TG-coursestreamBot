// internal/db/db_autodelivery.go
//
// Auto-delivery: user ne bot /start kela (kiva Telegram link kela) ki tyache sarv
// kharedi kelele courses (LOG_CHANNEL madhla cached content) aapoaap folder-wise
// pathavle jatat. Je lecture LOG madhe ajun nahit tyanchya jaagi "pending" placeholder
// jaato ani nantar (old var /predownload zalyavar) to placeholder tyach jaagi edit hoto.
//
// Collections:
//   auto_delivery_users    - kon /start kela (fakt tyannach delivery)
//   auto_delivery_courses  - (user, course) delivery chalu/purna - "fakt ekda" guarantee
//   auto_delivery_items    - pratyek message: header / delivered / pending
package db

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	ADStateHeader    = "header"
	ADStateDelivered = "delivered"
	ADStatePending   = "pending"
	ADStateRaw       = "raw" // watermark nasleli original copy pathavli; watermarked copy aalyavar replace hoil
	ADStateSending   = "sending" // pathavnyacha "haqq" ghetla (claim) -- ajun purna nahi
	ADStateGone      = "gone"    // user ne message delete kela
	ADStateExpired   = "expired" // access sampla
)

type ADUser struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	ChatID    int64              `bson:"chat_id"`
	StartedAt time.Time          `bson:"started_at"`
	Confirmed bool               `bson:"confirmed"`   // user ne popup madhe "Ho" dabla
	Declined  bool               `bson:"declined"`    // user ne "Nako" dabla
	PromptedAt time.Time         `bson:"prompted_at"` // popup kevha pathavla (spam tala)

	SummaryMsgID int  `bson:"summary_msg_id"` // user chat madhla "live" progress/summary message
	SummaryFinal bool `bson:"summary_final"`  // sarv lectures aale -> update thambva
	SummaryState string `bson:"summary_state"` // progress | sent | final (restart/duplicate admin msg tala)
	SentCount    int  `bson:"sent_count"`     // admin update (dar 20) sathi
}

type ADCourse struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"`
	Done      bool               `bson:"done"`
	Summarized bool              `bson:"summarized"`
	CreatedAt time.Time          `bson:"created_at"`
}

type ADItem struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"`
	MessageID int                `bson:"message_id"` // course file msg id; header = negative number
	ChatID    int64              `bson:"chat_id"`
	SentMsgID int                `bson:"sent_msg_id"`
	State     string             `bson:"state"`
	Title     string             `bson:"title,omitempty"`
	CheckedAt time.Time          `bson:"checked_at"`
	UpdatedAt time.Time          `bson:"updated_at"`
}

func adUsers() *mongo.Collection   { return Database.Collection("auto_delivery_users") }
func adCourses() *mongo.Collection { return Database.Collection("auto_delivery_courses") }
func adItems() *mongo.Collection   { return Database.Collection("auto_delivery_items") }

var adIndexOnce sync.Once

// EnsureAutoDeliveryIndexes -- unique indexes (ekda).
func EnsureAutoDeliveryIndexes() {
	adIndexOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = adUsers().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetUnique(true)})
		_, _ = adCourses().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "course_id", Value: 1}}, Options: options.Index().SetUnique(true)})
		_, _ = adItems().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "course_id", Value: 1}, {Key: "message_id", Value: 1}}, Options: options.Index().SetUnique(true)})
		_, _ = adItems().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "state", Value: 1}, {Key: "checked_at", Value: 1}}})
	})
}

func GetADUser(userID primitive.ObjectID) *ADUser {
	ctx, cancel := Ctx()
	defer cancel()
	var u ADUser
	if err := adUsers().FindOne(ctx, bson.M{"user_id": userID}).Decode(&u); err != nil {
		return nil
	}
	return &u
}

// MarkADPrompted -- popup pathavla (re-prompt spam tala).
func MarkADPrompted(userID primitive.ObjectID, chatID int64) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{
			"$set":         bson.M{"prompted_at": time.Now(), "chat_id": chatID},
			"$setOnInsert": bson.M{"user_id": userID, "confirmed": false, "declined": false, "started_at": time.Now()},
		}, options.Update().SetUpsert(true))
}

// SetADUserConfirmed -- user ne "sarv courses pathava" confirm kela -> delivery suru.
func SetADUserConfirmed(userID primitive.ObjectID, chatID int64) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{
			"$set":         bson.M{"confirmed": true, "declined": false, "chat_id": chatID, "started_at": time.Now()},
			"$setOnInsert": bson.M{"user_id": userID},
		}, options.Update().SetUpsert(true))
}

func SetADUserDeclined(userID primitive.ObjectID, chatID int64) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{
			"$set":         bson.M{"confirmed": false, "declined": true, "chat_id": chatID},
			"$setOnInsert": bson.M{"user_id": userID, "started_at": time.Now()},
		}, options.Update().SetUpsert(true))
}

// UserHasSentLectures -- ya bot ne aadhi hi user la kahi lecture pathavle aahet ka.
func UserHasSentLectures(userID primitive.ObjectID, botID int64) bool {
	ctx, cancel := Ctx()
	defer cancel()
	n, err := colSentLectureMsgs().CountDocuments(ctx, bson.M{"user_id": userID, "bot_id": botID}, options.Count().SetLimit(1))
	return err == nil && n > 0
}

func ListAutoDeliveryUsers() ([]ADUser, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := adUsers().Find(ctx, bson.M{"confirmed": true}, options.Find().SetSort(bson.D{{Key: "started_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []ADUser
	return out, cur.All(ctx, &out)
}

// ADCourseDone -- true = ya (user, course) chi delivery purna zali (parat nako).
func ADCourseDone(userID, courseID primitive.ObjectID) bool {
	ctx, cancel := Ctx()
	defer cancel()
	var c ADCourse
	if err := adCourses().FindOne(ctx, bson.M{"user_id": userID, "course_id": courseID}).Decode(&c); err != nil {
		return false
	}
	return c.Done
}

func EnsureADCourse(userID, courseID primitive.ObjectID) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adCourses().UpdateOne(ctx,
		bson.M{"user_id": userID, "course_id": courseID},
		bson.M{"$setOnInsert": bson.M{"user_id": userID, "course_id": courseID, "done": false, "summarized": false, "created_at": time.Now()}},
		options.Update().SetUpsert(true))
}

func MarkADCourseDone(userID, courseID primitive.ObjectID) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adCourses().UpdateOne(ctx, bson.M{"user_id": userID, "course_id": courseID}, bson.M{"$set": bson.M{"done": true}})
}

func ADItemExists(userID, courseID primitive.ObjectID, messageID int) bool {
	ctx, cancel := Ctx()
	defer cancel()
	n, err := adItems().CountDocuments(ctx, bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID})
	return err == nil && n > 0
}

// GetADItem -- (user, course, lecture) cha item (nasel tar nil).
func GetADItem(userID, courseID primitive.ObjectID, messageID int) *ADItem {
	ctx, cancel := Ctx()
	defer cancel()
	var it ADItem
	if err := adItems().FindOne(ctx, bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID}).Decode(&it); err != nil {
		return nil
	}
	return &it
}

func SaveADItem(it ADItem) error {
	ctx, cancel := Ctx()
	defer cancel()
	now := time.Now()
	_, err := adItems().UpdateOne(ctx,
		bson.M{"user_id": it.UserID, "course_id": it.CourseID, "message_id": it.MessageID},
		bson.M{"$set": bson.M{
			"chat_id": it.ChatID, "sent_msg_id": it.SentMsgID, "state": it.State,
			"title": it.Title, "updated_at": now, "checked_at": now,
		}},
		options.Update().SetUpsert(true))
	return err
}

// ListPendingADItems -- sarvat jasta velapasun check na kelele pending items.
func ListPendingADItems(limit int64) ([]ADItem, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := adItems().Find(ctx, bson.M{"state": bson.M{"$in": []string{ADStatePending, ADStateRaw}}},
		options.Find().SetSort(bson.D{{Key: "checked_at", Value: 1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []ADItem
	return out, cur.All(ctx, &out)
}

func TouchADItem(id primitive.ObjectID) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adItems().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"checked_at": time.Now()}})
}

func SetADItemState(id primitive.ObjectID, state string, sentMsgID int) {
	ctx, cancel := Ctx()
	defer cancel()
	set := bson.M{"state": state, "updated_at": time.Now(), "checked_at": time.Now()}
	if sentMsgID != 0 {
		set["sent_msg_id"] = sentMsgID
	}
	_, _ = adItems().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
}

// ListUnsummarizedADCourses -- delivery purna zali pan summary ajun pathavli nahi.
func ListUnsummarizedADCourses(userID primitive.ObjectID) []ADCourse {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := adCourses().Find(ctx, bson.M{"user_id": userID, "done": true, "summarized": bson.M{"$ne": true}})
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []ADCourse
	_ = cur.All(ctx, &out)
	return out
}

func MarkADSummarized(userID primitive.ObjectID, courseIDs []primitive.ObjectID) {
	if len(courseIDs) == 0 {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adCourses().UpdateMany(ctx, bson.M{"user_id": userID, "course_id": bson.M{"$in": courseIDs}},
		bson.M{"$set": bson.M{"summarized": true}})
}

// CountADItems -- ya (user, course) che lectures: delivered ani pending (headers vaglun).
func CountADItems(userID, courseID primitive.ObjectID) (delivered, pending int) {
	ctx, cancel := Ctx()
	defer cancel()
	d, _ := adItems().CountDocuments(ctx, bson.M{"user_id": userID, "course_id": courseID, "state": ADStateDelivered})
	p, _ := adItems().CountDocuments(ctx, bson.M{"user_id": userID, "course_id": courseID, "state": bson.M{"$in": []string{ADStatePending, ADStateRaw}}})
	return int(d), int(p)
}

// SetADSummaryMsg -- navin live summary message (navin batch). Final reset.
func SetADSummaryMsg(userID primitive.ObjectID, msgID int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID},
		bson.M{"$set": bson.M{"summary_msg_id": msgID, "summary_final": false}})
}

func SetADSummaryFinal(userID primitive.ObjectID) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"summary_final": true}})
}

// IncADSentCount -- user cha pathavlele/aalele lecture counter ++ ; navin value return.
func IncADSentCount(userID primitive.ObjectID) int {
	ctx, cancel := Ctx()
	defer cancel()
	var u ADUser
	err := adUsers().FindOneAndUpdate(ctx, bson.M{"user_id": userID},
		bson.M{"$inc": bson.M{"sent_count": 1}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&u)
	if err != nil {
		return 0
	}
	return u.SentCount
}

// ListADCoursesForUser -- ya user che sarv auto-delivery courses (done/ not done).
func ListADCoursesForUser(userID primitive.ObjectID) []ADCourse {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := adCourses().Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []ADCourse
	_ = cur.All(ctx, &out)
	return out
}

// ClaimADItem -- (user, course, message) pathavnyacha "haqq" atomically ghya.
// true = tumhi pathava. false = aadhich purna zala / dusra instance pathavtoy.
// 10 minutanpekshaa junya adaklelya ("sending") claim punha ghetla jato (crash/redeploy nantar).
func ClaimADItem(userID, courseID primitive.ObjectID, messageID int, chatID int64) bool {
	ctx, cancel := Ctx()
	defer cancel()
	now := time.Now()
	_, err := adItems().InsertOne(ctx, ADItem{
		UserID: userID, CourseID: courseID, MessageID: messageID, ChatID: chatID,
		State: ADStateSending, UpdatedAt: now, CheckedAt: now,
	})
	if err == nil {
		return true
	}
	if !mongo.IsDuplicateKeyError(err) {
		return false
	}
	res := adItems().FindOneAndUpdate(ctx,
		bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID,
			"state": ADStateSending, "updated_at": bson.M{"$lt": now.Add(-10 * time.Minute)}},
		bson.M{"$set": bson.M{"updated_at": now}})
	return res.Err() == nil
}

// ReleaseADItem -- pathavta aale nahi (flood/error) -> claim soda mhanje punha prayatna hoil.
func ReleaseADItem(userID, courseID primitive.ObjectID, messageID int) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adItems().DeleteOne(ctx, bson.M{"user_id": userID, "course_id": courseID, "message_id": messageID, "state": ADStateSending})
}

// TrySetADSummaryMsg -- live summary message ID fakt jar ajun nasel kiva junya batch cha final zala asel tarach set.
func TrySetADSummaryMsg(userID primitive.ObjectID, msgID int) bool {
	ctx, cancel := Ctx()
	defer cancel()
	res, err := adUsers().UpdateOne(ctx,
		bson.M{"user_id": userID, "$or": bson.A{
			bson.M{"summary_msg_id": 0},
			bson.M{"summary_msg_id": bson.M{"$exists": false}},
			bson.M{"summary_final": true},
		}},
		bson.M{"$set": bson.M{"summary_msg_id": msgID, "summary_final": false, "summary_state": "progress"}})
	return err == nil && res.MatchedCount > 0
}

func SetADSummaryState(userID primitive.ObjectID, state string) {
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = adUsers().UpdateOne(ctx, bson.M{"user_id": userID}, bson.M{"$set": bson.M{"summary_state": state}})
}
