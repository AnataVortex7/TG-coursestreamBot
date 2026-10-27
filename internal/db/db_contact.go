// internal/db/db_contact.go
//
// Contact-gate (bot /start वर mobile verify) साठी helper queries.
package db

import (
	"errors"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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


// ─── Gate contacts: pahila message + verified mobile (registration adhura rahila tari harvat nahi) ───

var gateContactsIdxOnce sync.Once

func colGateContacts() *mongo.Collection {
	// tg_id unique — ek-vela jast messages (race) aale tari ekach doc / ekach "navin user" notification.
	gateContactsIdxOnce.Do(func() {
		ctx, cancel := Ctx()
		defer cancel()
		_, _ = Database.Collection("gate_contacts").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "tg_id", Value: 1}}, Options: options.Index().SetUnique(true)})
	})
	return Database.Collection("gate_contacts")
}

// GateContact — bot var pahilyanda alelya (unregistered) user chi nond.
type GateContact struct {
	TGID       int64     `bson:"tg_id"`
	Name       string    `bson:"name"`
	Username   string    `bson:"username"`
	FirstMsg   string    `bson:"first_msg"`
	FirstAt    time.Time `bson:"first_at"`
	Mobile     string    `bson:"mobile"`
	VerifiedAt time.Time `bson:"verified_at"`
	Registered bool      `bson:"registered"`
}

// RecordFirstContact — pahilyanda ch true (isNew). Pahila message + naav + username sathavto.
func RecordFirstContact(tgID int64, name, username, firstMsg string) (bool, error) {
	if tgID == 0 {
		return false, nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	res, err := colGateContacts().UpdateOne(ctx,
		bson.M{"tg_id": tgID},
		bson.M{"$setOnInsert": bson.M{"name": name, "username": username, "first_msg": firstMsg, "first_at": time.Now(), "registered": false}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil // dusrya goroutine ne aadhich nond keli
		}
		return false, err
	}
	return res.UpsertedCount > 0, nil
}

// SaveVerifiedContact — user ne contact-share kela (verified mobile) — registration purna zala nasla tari DB madhe rahto.
func SaveVerifiedContact(tgID int64, mobile, name string) {
	if tgID == 0 || mobile == "" {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	for try := 0; try < 3; try++ {
		_, err := colGateContacts().UpdateOne(ctx,
			bson.M{"tg_id": tgID},
			bson.M{"$set": bson.M{"mobile": mobile, "verified_at": time.Now(), "name": name},
				"$setOnInsert": bson.M{"first_at": time.Now(), "registered": false}},
			options.Update().SetUpsert(true),
		)
		if err == nil || !mongo.IsDuplicateKeyError(err) {
			return
		}
	}
}

func MarkContactRegistered(tgID int64) {
	if tgID == 0 {
		return
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, _ = colGateContacts().UpdateOne(ctx, bson.M{"tg_id": tgID}, bson.M{"$set": bson.M{"registered": true}})
}

func GetGateContact(tgID int64) *GateContact {
	ctx, cancel := Ctx()
	defer cancel()
	var c GateContact
	if err := colGateContacts().FindOne(ctx, bson.M{"tg_id": tgID}).Decode(&c); err != nil {
		return nil
	}
	return &c
}

// ListUnregisteredVerifiedContacts — mobile verify zala pan registration purna na zaleli.
func ListUnregisteredVerifiedContacts(limit int) []GateContact {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colGateContacts().Find(ctx,
		bson.M{"mobile": bson.M{"$ne": ""}, "registered": bson.M{"$ne": true}},
		options.Find().SetLimit(int64(limit)))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []GateContact
	_ = cur.All(ctx, &out)
	return out
}
