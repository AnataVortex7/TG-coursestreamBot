// internal/db/db_colab_poll.go
//
// Colab/watermark worker cha "online" status आधी फक्त एका process cha
// in-memory map मध्ये track व्हायचा — जर main-server multiple replicas/
// containers वर चालत असेल (उदा. HF Spaces कधीकधी असं करतं), तर एका replica
// ला poll मिळाला तरी दुसऱ्या replica ला (जो admin चा /colabstatus command
// handle करतोय) तो कधीच दिसायचा नाही — "online असूनही कायम offline दिसणं"
// हा bug यातूनच आला. इथे तेच state DB मध्ये (सगळ्या replicas मध्ये shared)
// ठेवलं आहे, त्यामुळे कुठलाही replica नेहमी बरोबर स्थिती दाखवेल.
package db

import (
	"errors"
	"time"

	"EverythingSuckz/fsb/config"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ColabWorkerPoll struct {
	ID       string    `bson:"_id"` // botToken + ":" + workerID
	WorkerID string    `bson:"worker_id"`
	BotToken string    `bson:"bot_token"`
	LastPoll time.Time `bson:"last_poll"`
}

func colColabWorkerPolls() *mongo.Collection { return Database.Collection("colab_worker_polls") }

func RecordColabPoll(workerID string) error {
	if workerID == "" {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	botToken := config.ValueOf.BotToken
	docID := botToken + ":" + workerID

	_, err := colColabWorkerPolls().UpdateOne(ctx,
		bson.M{"_id": docID},
		bson.M{"$set": bson.M{
			"worker_id": workerID,
			"bot_token": botToken,
			"last_poll": time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}

func IsColabOnlineDB(threshold time.Duration) (bool, error) {
	ctx, cancel := Ctx()
	defer cancel()

	count, err := colColabWorkerPolls().CountDocuments(ctx, bson.M{
		"bot_token": config.ValueOf.BotToken,
		"last_poll": bson.M{"$gt": time.Now().Add(-threshold)},
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func ActiveColabWorkersDB(threshold time.Duration) ([]string, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Clean up ALL stale ghost worker records older than threshold (cross-bot cleanup)
	_, _ = colColabWorkerPolls().DeleteMany(ctx, bson.M{
		"last_poll": bson.M{"$lte": time.Now().Add(-threshold)},
	})

	cursor, err := colColabWorkerPolls().Find(ctx, bson.M{
		"bot_token": config.ValueOf.BotToken,
		"last_poll": bson.M{"$gt": time.Now().Add(-threshold)},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []ColabWorkerPoll
	if err := cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, w := range list {
		ids = append(ids, w.WorkerID)
	}
	return ids, nil
}

// LastColabPollAgoDB — सर्वात recent poll कधी झाला (कुठल्याही worker कडून). कधीच नसेल तर zero time.
func LastColabPollAgoDB() (time.Time, error) {
	ctx, cancel := Ctx()
	defer cancel()

	opts := options.FindOne().SetSort(bson.D{{Key: "last_poll", Value: -1}})
	var w ColabWorkerPoll
	err := colColabWorkerPolls().FindOne(ctx, bson.M{"bot_token": config.ValueOf.BotToken}, opts).Decode(&w)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return w.LastPoll, nil
}

// ForgetColabWorkerDB — worker बराच वेळ offline राहिला की record काढतो (cleanup).
func ForgetColabWorkerDB(workerID string) error {
	ctx, cancel := Ctx()
	defer cancel()
	
	botToken := config.ValueOf.BotToken
	docID := botToken + ":" + workerID
	_, err := colColabWorkerPolls().DeleteOne(ctx, bson.M{"_id": docID})
	return err
}
