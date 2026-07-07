// internal/urlmonitor/store.go
package urlmonitor

import (
	"context"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SaveURL — insert new MonitorURL into MongoDB
func SaveURL(url, label string, intervalSeconds int, token string) (*MonitorURL, error) {
	if intervalSeconds < 2 {
		intervalSeconds = 2
	}
	doc := &MonitorURL{
		ID:              primitive.NewObjectID(),
		URL:             url,
		Label:           label,
		IntervalSeconds: intervalSeconds,
		Enabled:         true,
		CreatedAt:       time.Now(),
		Token:           token,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ColURLMonitors.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// DeleteURL — remove MonitorURL from MongoDB by ID
func DeleteURL(id primitive.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ColURLMonitors.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// ListURLs — fetch all MonitorURLs from MongoDB
func ListURLs() ([]MonitorURL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := db.ColURLMonitors.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var results []MonitorURL
	if err := cursor.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetURLByID — fetch single MonitorURL by ObjectID
func GetURLByID(id primitive.ObjectID) (*MonitorURL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var doc MonitorURL
	err := db.ColURLMonitors.FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// UpdateInterval — change polling interval for existing URL
func UpdateInterval(id primitive.ObjectID, intervalSeconds int) error {
	if intervalSeconds < 2 {
		intervalSeconds = 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ColURLMonitors.UpdateOne(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"interval_seconds": intervalSeconds}},
	)
	return err
}
