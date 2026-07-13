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

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ColabWorkerPoll struct {
	WorkerID string    `bson:"_id"`
	LastPoll time.Time `bson:"last_poll"`
}

func colColabWorkerPolls() *mongo.Collection { return Database.Collection("colab_worker_polls") }

// RecordColabPoll — प्रत्येक poll ला call करा (best-effort — error आला तरी
// caller ने HTTP response block करू नये, फक्त log करावं).
func RecordColabPoll(workerID string) error {
	if workerID == "" {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	_, err := colColabWorkerPolls().UpdateOne(ctx,
		bson.M{"_id": workerID},
		bson.M{"$set": bson.M{"last_poll": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// IsColabOnlineDB — कुठलाही 1 worker तरी threshold च्या आत poll करून गेला आहे का.
func IsColabOnlineDB(threshold time.Duration) (bool, error) {
	ctx, cancel := Ctx()
	defer cancel()

	count, err := colColabWorkerPolls().CountDocuments(ctx, bson.M{
		"last_poll": bson.M{"$gt": time.Now().Add(-threshold)},
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ActiveColabWorkersDB — सध्या online (threshold च्या आत poll केलेले) worker IDs.
func ActiveColabWorkersDB(threshold time.Duration) ([]string, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := colColabWorkerPolls().Find(ctx, bson.M{
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
	err := colColabWorkerPolls().FindOne(ctx, bson.M{}, opts).Decode(&w)
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
	_, err := colColabWorkerPolls().DeleteOne(ctx, bson.M{"_id": workerID})
	return err
}
