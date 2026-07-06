// internal/db/db_folders.go
package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Create ──────────────────────────────────────────────────────────────────

// CreateFolder — नवीन folder तयार करतो.
// Order automatically शेवटी assign होतो (max+1).
func CreateFolder(courseID primitive.ObjectID, name string, ranges []FolderRange, messages []int) (*CourseFolder, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Current max order शोधतो
	maxOrder := 0
	opts := options.FindOne().SetSort(bson.M{"order": -1})
	var last CourseFolder
	err := ColFolders.FindOne(ctx, bson.M{"course_id": courseID}, opts).Decode(&last)
	if err == nil {
		maxOrder = last.Order
	}

	if messages == nil {
		messages = []int{}
	}
	folder := CourseFolder{
		ID:        primitive.NewObjectID(),
		CourseID:  courseID,
		Name:      name,
		Ranges:    ranges,
		Messages:  messages,
		Order:     maxOrder + 1,
		CreatedAt: time.Now(),
	}

	_, err = ColFolders.InsertOne(ctx, folder)
	if err != nil {
		return nil, err
	}
	return &folder, nil
}

// ─── Read ─────────────────────────────────────────────────────────────────────

// GetFoldersForCourse — course च्या सर्व folders, display order नुसार.
func GetFoldersForCourse(courseID primitive.ObjectID) ([]CourseFolder, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColFolders.Find(ctx,
		bson.M{"course_id": courseID},
		options.Find().SetSort(bson.M{"order": 1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var folders []CourseFolder
	if err := cursor.All(ctx, &folders); err != nil {
		return nil, err
	}
	return folders, nil
}

// GetFolderByID — एक specific folder fetch करतो.
func GetFolderByID(folderID primitive.ObjectID) (*CourseFolder, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var folder CourseFolder
	err := ColFolders.FindOne(ctx, bson.M{"_id": folderID}).Decode(&folder)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &folder, nil
}

// ─── Update ───────────────────────────────────────────────────────────────────

// UpdateFolder — folder चे name आणि ranges update करतो.
func UpdateFolder(folderID primitive.ObjectID, name string, ranges []FolderRange, messages []int) error {
	ctx, cancel := Ctx()
	defer cancel()

	if messages == nil {
		messages = []int{}
	}
	_, err := ColFolders.UpdateOne(ctx,
		bson.M{"_id": folderID},
		bson.M{"$set": bson.M{
			"name":     name,
			"ranges":   ranges,
			"messages": messages,
		}},
	)
	return err
}

// ReorderFolders — folders चा display order update करतो.
// orders = map[folderID hex] → new order number
func ReorderFolders(courseID primitive.ObjectID, orders map[string]int) error {
	ctx, cancel := Ctx()
	defer cancel()

	for idHex, order := range orders {
		folderID, err := primitive.ObjectIDFromHex(idHex)
		if err != nil {
			continue
		}
		_, err = ColFolders.UpdateOne(ctx,
			bson.M{"_id": folderID, "course_id": courseID},
			bson.M{"$set": bson.M{"order": order}},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// ─── Delete ───────────────────────────────────────────────────────────────────

// DeleteFolder — एक folder delete करतो.
// Files delete होत नाहीत — त्या Ungrouped मध्ये जातात.
func DeleteFolder(folderID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColFolders.DeleteOne(ctx, bson.M{"_id": folderID})
	return err
}

// DeleteAllFoldersForCourse — course delete झाल्यावर सर्व folders काढतो.
func DeleteAllFoldersForCourse(courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColFolders.DeleteMany(ctx, bson.M{"course_id": courseID})
	return err
}

// ─── Helper ───────────────────────────────────────────────────────────────────

// FolderForSortOrder — एखाद्या sort_order साठी कोणता folder आहे ते शोधतो.
// नाही मिळाला तर nil — म्हणजे Ungrouped.
func FolderForSortOrder(folders []CourseFolder, sortOrder int) *CourseFolder {
	for i := range folders {
		for _, r := range folders[i].Ranges {
			if sortOrder >= r.Start && sortOrder <= r.End {
				return &folders[i]
			}
		}
		for _, m := range folders[i].Messages {
			if m == sortOrder {
				return &folders[i]
			}
		}
	}
	return nil
}
