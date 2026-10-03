// internal/db/db_userbot_creds.go
//
// Userbot session सोबत वापरलेले API ID / API Hash. Login करताना admin ने manual
// दिले असतील तर session चालू असेपर्यंत (logout पर्यंत) तेच वापरले जातात —
// restart नंतरही आणि सगळ्या processes मध्ये. Logout वर session सोबत हे पण delete होतात.
package db

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"EverythingSuckz/fsb/config"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func colUserbotCreds() *mongo.Collection { return Database.Collection("userbot_creds") }

// session string दुसऱ्यांदा store करायची गरज नाही — त्याचा hash key म्हणून वापरतो.
func sessionCredsKey(sessionStr string) string {
	sum := sha256.Sum256([]byte(sessionStr))
	return hex.EncodeToString(sum[:])
}

// SaveSessionCreds — या session ने login करताना वापरलेले API ID/Hash save करतो.
func SaveSessionCreds(sessionStr string, apiID int, apiHash string) error {
	if sessionStr == "" || apiID == 0 || apiHash == "" {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colUserbotCreds().UpdateOne(ctx,
		bson.M{"session_key": sessionCredsKey(sessionStr)},
		bson.M{"$set": bson.M{"api_id": apiID, "api_hash": apiHash, "saved_at": time.Now()}},
		options.Update().SetUpsert(true),
	)
	return err
}

// SessionAPICreds — या session साठी API ID/Hash. Save केलेले नसतील तर global config.
func SessionAPICreds(sessionStr string) (int, string) {
	globalID, globalHash := int(config.ValueOf.ApiID), config.ValueOf.ApiHash
	if sessionStr == "" {
		return globalID, globalHash
	}
	ctx, cancel := Ctx()
	defer cancel()
	var doc struct {
		ApiID   int    `bson:"api_id"`
		ApiHash string `bson:"api_hash"`
	}
	err := colUserbotCreds().FindOne(ctx, bson.M{"session_key": sessionCredsKey(sessionStr)}).Decode(&doc)
	if err != nil || doc.ApiID == 0 || doc.ApiHash == "" {
		return globalID, globalHash
	}
	return doc.ApiID, doc.ApiHash
}

// DeleteSessionCreds — logout वर call होतो.
func DeleteSessionCreds(sessionStr string) error {
	if sessionStr == "" {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colUserbotCreds().DeleteOne(ctx, bson.M{"session_key": sessionCredsKey(sessionStr)})
	return err
}
