// Package instancelock — MongoDB-based "single active instance" lease.
//
// Samasya: redeploy dara jaruna + navin instance kahi kaal ekach vela healthy
// astat. Doghanni ek-ch userbot session / bot token vaparla ki Telegram
// AUTH_KEY_DUPLICATED deto ani session kharab hoto.
//
// Upay: Telegram var kahihi connect karnyaadhi navin instance ya lease cha wait
// karto. Jaruna instance SIGTERM var lease release karto (navin 1-2 sec madhe
// start hoto). Jaruna instance crash/kill zala tar lease TTL (30s) nantar sutate.
// Heartbeat fail zala (lease dusryakade gela) tar instance swatah exit hoto.
package instancelock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

const (
	leaseTTL      = 30 * time.Second
	renewEvery    = 8 * time.Second
	acquirePoll   = 2 * time.Second
	maxRenewFails = 3
)

// leaseKey - INSTANCE_LOCK_KEY env se badalta yeto. Jar tumchyakade Old + New
// machine janiv-purvak ekach DB var doghe chalavayche asatil (vegvegle userbot
// session key sobat), tar doghanna vegle key dya mhanje ek dusryachi wait baghat nahi.
var leaseKey = func() string {
	if v := os.Getenv("INSTANCE_LOCK_KEY"); v != "" {
		return v
	}
	return "active_instance"
}()

// Enabled - INSTANCE_LOCK=false/off/0 dila tar lock purnapane band (junya sarkhe vartan).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("INSTANCE_LOCK"))) {
	case "false", "off", "0", "no":
		return false
	}
	return true
}

var (
	// ID — ya process cha unique id.
	ID = newID()

	mu       sync.Mutex
	held     bool
	stopBeat context.CancelFunc
)

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	host, _ := os.Hostname()
	return host + "-" + hex.EncodeToString(b)
}

func col() *mongo.Collection { return db.Database.Collection("instance_lock") }

// tryAcquire — lease free asel, expire zala asel kiva aaplich asel tar true.
func tryAcquire(ctx context.Context) (bool, error) {
	now := time.Now()
	filter := bson.M{
		"_id": leaseKey,
		"$or": bson.A{
			bson.M{"owner": ID},
			bson.M{"expires_at": bson.M{"$lte": now}},
		},
	}
	update := bson.M{"$set": bson.M{
		"owner":      ID,
		"expires_at": now.Add(leaseTTL),
		"updated_at": now,
	}}
	_, err := col().UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	if err == nil {
		return true, nil
	}
	// Upsert + filter mismatch => _id duplicate => lease dusryakade aahe.
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	return false, err
}

// Acquire — lease milepryant block karto. ctx cancel zala tar error.
func Acquire(ctx context.Context, log *zap.Logger) error {
	log = log.Named("instancelock")
	if !Enabled() {
		log.Warn("INSTANCE_LOCK band aahe — duplicate instance pasun sanrakshan nahi")
		return nil
	}
	first := true
	for {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		ok, err := tryAcquire(cctx)
		cancel()
		if err != nil {
			log.Warn("lease check error — retry", zap.Error(err))
		} else if ok {
			mu.Lock()
			held = true
			mu.Unlock()
			log.Info("Active-instance lease mila — ha instance aata PRIMARY aahe", zap.String("id", ID))
			startHeartbeat(log)
			return nil
		} else if first {
			log.Info("Dusra instance active aahe — STANDBY (Telegram connect karnar nahi), lease cha wait", zap.String("id", ID))
			first = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(acquirePoll):
		}
	}
}

func startHeartbeat(log *zap.Logger) {
	hctx, cancel := context.WithCancel(context.Background())
	mu.Lock()
	stopBeat = cancel
	mu.Unlock()
	go func() {
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		fails := 0
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
			}
			cctx, c := context.WithTimeout(hctx, 8*time.Second)
			ok, err := tryAcquire(cctx)
			c()
			switch {
			case err != nil:
				// DB blip — TTL cha andaaj thevun thoda sahan karto
				fails++
				log.Warn("lease renew error", zap.Int("fails", fails), zap.Error(err))
				if fails >= maxRenewFails {
					log.Error("lease renew satat fail — duplicate session tala mhanun exit")
					os.Exit(1)
				}
			case !ok:
				log.Error("Lease dusryakade geli — ha instance exit hoto (AUTH_KEY_DUPLICATED tala)")
				os.Exit(0)
			default:
				fails = 0
			}
		}
	}()
}

// Release — graceful shutdown var call kara; navin instance la lagech sandhi.
func Release() {
	mu.Lock()
	if stopBeat != nil {
		stopBeat()
		stopBeat = nil
	}
	wasHeld := held
	held = false
	mu.Unlock()
	if !wasHeld || db.Database == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := col().UpdateOne(ctx,
		bson.M{"_id": leaseKey, "owner": ID},
		bson.M{"$set": bson.M{"expires_at": time.Unix(0, 0), "updated_at": time.Now()}},
	)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		zap.L().Warn("lease release error", zap.Error(err))
	}
}

// IsPrimary — ha instance sadhya active aahe ka.
func IsPrimary() bool {
	mu.Lock()
	defer mu.Unlock()
	return held
}
