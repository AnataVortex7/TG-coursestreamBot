// internal/db/db_tgsweep.go
//
// Old + new server EKACH MongoDB vaparatat.
//   • NEW server user cha real Telegram ID  `tg_uid`      field madhe vachto/lihito.
//   • OLD server (code badlata yet nahi) `telegram_id`    field vachto.
// Mhanun `telegram_id` madhe sarvanna FAKE (kadhich real navhe asa) ID thevto —
// mag OLD bot kunalach ओळखat nahi => old varun kunala course content jat nahi.
// Real ID kayam `tg_uid` madhe surakshit.
//
// Admin (ALLOWED_USERS) ani userbot_access असलेले docs: telegram_id REAL rahato
// (old var admin/heavy kaam saathi).
package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

// FakeTGBase — real Telegram IDs (< ~1e13) peksha khup motha. >= he fake aahe.
const FakeTGBase = int64(1) << 62

// FakeTGIDFor — user _id varun deterministic, unique fake ID (positive, FakeTGBase+).
func FakeTGIDFor(userID primitive.ObjectID) int64 {
	var v int64
	for _, b := range userID[5:12] { // 7 bytes = 56 bit
		v = v<<8 | int64(b)
	}
	return FakeTGBase + v
}

// SweepTelegramIDs — real `telegram_id` असलेले सगळे users (admin/userbot सोडून)
// -> tg_uid madhe copy + telegram_id = fake. Returns: kiti users spoof zale.
func SweepTelegramIDs(keepReal []int64) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	realRange := bson.M{"$gt": 0, "$lt": FakeTGBase}
	keep := keepReal
	if keep == nil {
		keep = []int64{}
	}

	// 1) Admin / userbot docs: telegram_id real rahato, fakt tg_uid set kara.
	_, err := ColUsers.UpdateMany(ctx,
		bson.M{
			"telegram_id": realRange,
			"tg_uid":      bson.M{"$exists": false},
			"$or": bson.A{
				bson.M{"telegram_id": bson.M{"$in": keep}},
				bson.M{"userbot_access": true},
			},
		},
		mongo.Pipeline{bson.D{{Key: "$set", Value: bson.D{{Key: "tg_uid", Value: "$telegram_id"}}}}},
	)
	if err != nil {
		return 0, err
	}

	// 2) Baki sagle: spoof.
	cur, err := ColUsers.Find(ctx, bson.M{
		"telegram_id":    realRange,
		"userbot_access": bson.M{"$ne": true},
	})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	keepSet := map[int64]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}

	n := 0
	for cur.Next(ctx) {
		var u struct {
			ID         primitive.ObjectID `bson:"_id"`
			TelegramID int64              `bson:"telegram_id"`
			TgUID      int64              `bson:"tg_uid"`
		}
		if err := cur.Decode(&u); err != nil || keepSet[u.TelegramID] {
			continue
		}
		set := bson.M{"telegram_id": FakeTGIDFor(u.ID)}
		if u.TgUID <= 0 {
			set["tg_uid"] = u.TelegramID // real ID surakshit karun mag spoof
		}
		// filter madhe telegram_id real -> concurrent change safe
		if _, err := ColUsers.UpdateOne(ctx,
			bson.M{"_id": u.ID, "telegram_id": u.TelegramID},
			bson.M{"$set": set}); err == nil {
			n++
		}
	}
	if err := cur.Err(); err != nil {
		return n, err
	}

	// 3) telegram_id 0 / missing (Telegram adhi link nahi kelele navin website users):
	//    tyannahi fake ID — jenekarun old /predownload "link nahi" mhanat nahi.
	zero := bson.A{
		bson.M{"telegram_id": bson.M{"$exists": false}},
		bson.M{"telegram_id": 0},
	}
	cur2, err := ColUsers.Find(ctx, bson.M{"$or": zero}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return n, err
	}
	defer cur2.Close(ctx)
	for cur2.Next(ctx) {
		var u struct {
			ID primitive.ObjectID `bson:"_id"`
		}
		if cur2.Decode(&u) != nil {
			continue
		}
		if _, err := ColUsers.UpdateOne(ctx,
			bson.M{"_id": u.ID, "$or": zero},
			bson.M{"$set": bson.M{"telegram_id": FakeTGIDFor(u.ID)}}); err == nil {
			n++
		}
	}
	return n, cur2.Err()
}

// StartTGIDSweeper — startup la ani nantar dar 2 minutes. Konihi (old bot var
// chukun) link zala tari lagech spoof hoto => expiry kunacha skip hot nahi.
func StartTGIDSweeper(ctx context.Context, keepReal []int64, log *zap.Logger) {
	log = log.Named("tgid_sweeper")
	first := true
	run := func() {
		n, err := SweepTelegramIDs(keepReal)
		switch {
		case err != nil:
			log.Warn("sweep failed", zap.Error(err))
		case n > 0 || first:
			log.Sugar().Infof("sweep done: %d user(s) spoofed (admin/userbot docs real thevle)", n)
		}
		first = false
	}
	run()
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
