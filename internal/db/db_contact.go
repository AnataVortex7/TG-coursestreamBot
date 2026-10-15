// internal/db/db_contact.go
//
// Contact-gate (bot /start वर mobile verify) साठी helper queries.
package db

import (
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// GetUserByMobileVariants — mobile वेगवेगळ्या format मध्ये साठवलेला असू शकतो
// (9876543210 / 919876543210 / +919876543210). सगळ्या variants वर match करतो.
// (nil, nil) = सापडला नाही.
func GetUserByMobileVariants(variants []string) (*User, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	var user User
	err := ColUsers.FindOne(ctx, bson.M{"mobile": bson.M{"$in": variants}}).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// GetUserByEmailExact — फक्त `email` field वर match (username वर नाही).
// (nil, nil) = सापडला नाही.
func GetUserByEmailExact(email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	var user User
	err := ColUsers.FindOne(ctx, bson.M{"email": email}).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// SetUserEmailIfEmpty — account ला आधी email नसेल तरच set करतो.
// आधीचा email कधीच overwrite करत नाही.
func SetUserEmailIfEmpty(userID primitive.ObjectID, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("email रिकामा आहे")
	}
	ctx, cancel := Ctx()
	defer cancel()

	res, err := ColUsers.UpdateOne(ctx,
		bson.M{
			"_id": userID,
			"$or": bson.A{
				bson.M{"email": bson.M{"$exists": false}},
				bson.M{"email": ""},
			},
		},
		bson.M{"$set": bson.M{"email": email}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errors.New("हा email दुसऱ्या account ला आधीच जोडलेला आहे")
		}
		return err
	}
	if res.MatchedCount == 0 {
		return errors.New("या account ला आधीच email आहे")
	}
	return nil
}
