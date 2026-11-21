// internal/db/db_ghworkers.go
//
// GitHub Actions "workspaces" (एक workspace = एक GitHub account/token + एक repo).
// Token DB मध्ये encrypted (AES-GCM) साठवला जातो — plain token कधीच नाही.
package db

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ── Settings keys (app_settings collection) ──────────────────────────────────
const (
	SettingGHAutoEnabled   = "gh_auto_enabled"     // master switch (bool, default false)
	SettingGHDailyLimitMin = "gh_daily_limit_min"  // per-workspace daily cap (default 600 = 10h)
	SettingGHRunLimitMin   = "gh_run_limit_min"    // per-run cap (default 300 = 5h)
	SettingGHRestartWaitMn = "gh_restart_wait_min" // run limit नंतर किती वेळ थांबायचं (default 20)
	SettingGHIdleOffMin    = "gh_idle_off_min"     // jobs नसतील तर किती मिनिटांनी off (default 1, 0 = लगेच)
	SettingGHParallel      = "gh_parallel"         // एका workspace वर एकाच वेळी किती runs (default 2)
)

type GHWorkspace struct {
	ID        string    `bson:"_id" json:"id"`
	Label     string    `bson:"label" json:"label"`
	Owner     string    `bson:"owner" json:"owner"`
	Repo      string    `bson:"repo" json:"repo"`
	EventType string    `bson:"event_type" json:"event_type"`
	TokenEnc  string    `bson:"token_enc" json:"-"`
	Enabled   bool      `bson:"enabled" json:"enabled"` // watermark साठी निवडलेला workspace
	DailyLimitMin int64 `bson:"daily_limit_min" json:"daily_limit_min"` // 0 = global default वापर
	CreatedAt time.Time `bson:"created_at" json:"created_at"`

	// ── runtime state (orchestrator लिहितो) ─────────────────────────────
	Runs           []GHRunState `bson:"runs" json:"-"`
	BlockedUntil   time.Time `bson:"blocked_until" json:"-"`   // error मुळे थांबवलेलं (far-future = admin ने clear करेपर्यंत)
	BlockedReason  string    `bson:"blocked_reason" json:"blocked_reason"`
	FailStreak     int       `bson:"fail_streak" json:"-"`
	CooldownUntil  time.Time `bson:"cooldown_until" json:"cooldown_until"`
	LastDispatchAt time.Time `bson:"last_dispatch_at" json:"last_dispatch_at"`
	IdleSince      time.Time `bson:"idle_since" json:"-"`
	UsageDay       string    `bson:"usage_day" json:"usage_day"`
	UsedSeconds    int64     `bson:"used_seconds" json:"used_seconds"`
	LastEvent      string    `bson:"last_event" json:"last_event"`
	LastError      string    `bson:"last_error" json:"last_error"`
}

// GHRunState — आपण track करत असलेला एक चालू GitHub run.
type GHRunState struct {
	ID            int64     `bson:"id"`
	Start         time.Time `bson:"start"` // zero = अजून queued
	CancelAskedAt time.Time `bson:"cancel_asked_at"`
	SeenAt        time.Time `bson:"seen_at"` // आपल्याला पहिल्यांदा कधी दिसला (queued timeout साठी)
}

func colGHWorkspaces() *mongo.Collection { return Database.Collection("gh_workspaces") }
func colGHLease() *mongo.Collection      { return Database.Collection("gh_lease") }

func ListGHWorkspaces() ([]GHWorkspace, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colGHWorkspaces().Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []GHWorkspace
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func GetGHWorkspace(id string) (*GHWorkspace, error) {
	ctx, cancel := Ctx()
	defer cancel()
	var w GHWorkspace
	if err := colGHWorkspaces().FindOne(ctx, bson.M{"_id": id}).Decode(&w); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func InsertGHWorkspace(w *GHWorkspace) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colGHWorkspaces().InsertOne(ctx, w)
	return err
}

func DeleteGHWorkspace(id string) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colGHWorkspaces().DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// SetGHWorkspaceConfig — फक्त admin-editable fields (orchestrator चं runtime state overwrite होत नाही).
func SetGHWorkspaceConfig(id string, set bson.M) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colGHWorkspaces().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	return err
}

// SaveGHWorkspaceRuntime — फक्त runtime fields $set करतो (admin ने केलेला enabled/label बदल पुसला जात नाही).
func SaveGHWorkspaceRuntime(w *GHWorkspace) error {
	return SetGHWorkspaceConfig(w.ID, bson.M{
		"runs":             w.Runs,
		"blocked_until":    w.BlockedUntil,
		"blocked_reason":   w.BlockedReason,
		"fail_streak":      w.FailStreak,
		"cooldown_until":   w.CooldownUntil,
		"last_dispatch_at": w.LastDispatchAt,
		"idle_since":       w.IdleSince,
		"usage_day":        w.UsageDay,
		"used_seconds":     w.UsedSeconds,
		"last_event":       w.LastEvent,
		"last_error":       w.LastError,
	})
}

// AcquireGHLease — multiple replicas असतील तरी orchestrator एकाच ठिकाणी चालावा.
// Lease मिळाला तर true. (दुसऱ्याकडे valid lease असेल तर duplicate-key error → false.)
func AcquireGHLease(owner string, ttl time.Duration) bool {
	ctx, cancel := Ctx()
	defer cancel()
	now := time.Now()
	_, err := colGHLease().UpdateOne(ctx,
		bson.M{"_id": "orchestrator", "$or": bson.A{
			bson.M{"expires_at": bson.M{"$lt": now}},
			bson.M{"owner": owner},
		}},
		bson.M{"$set": bson.M{"owner": owner, "expires_at": now.Add(ttl)}},
		options.Update().SetUpsert(true),
	)
	return err == nil
}
