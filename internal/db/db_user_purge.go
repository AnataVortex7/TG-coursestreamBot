// internal/db/db_user_purge.go
//
// Admin panel varun user DELETE kela ki tyacha SARVA data DB madhun kayamcha
// (permanent) kadhun taktoo — "disable" nahi. User punha registration karu shakto
// (mobile/email unique index mokle hotat, nava user_id banto).
//
// Ordering mahatvacha: sarv related collections aadhi, `users` document SHEVTI.
// Madhech kahi fail zala tar users doc tasach rahto => admin panel madhe user
// disat rahto ani punha Delete dabun retry karta yeto (purge idempotent aahe).
package db

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrProtectedUser — admin / userbot-access doc delete karu naye.
var ErrProtectedUser = errors.New("admin / userbot-access account delete karta yet nahi")

// IsProtectedUserDoc — users collection madhe admin/userbot docs pan astat
// (SetUserbotAccess upsert karto). Te chukun delete hou naye.
func IsProtectedUserDoc(userID primitive.ObjectID) bool {
	ctx, cancel := Ctx()
	defer cancel()
	var doc struct {
		UserbotAccess bool `bson:"userbot_access"`
	}
	if err := ColUsers.FindOne(ctx, bson.M{"_id": userID}).Decode(&doc); err != nil {
		return false
	}
	return doc.UserbotAccess
}

// PurgeUserCompletely — user cha sarva data permanent delete.
// Returns: collection-wise deleted count (log/admin response sathi) + error.
func PurgeUserCompletely(u *User) (map[string]int64, error) {
	if u == nil {
		return nil, errors.New("user nil")
	}
	if IsProtectedUserDoc(u.ID) {
		return nil, ErrProtectedUser
	}

	counts := map[string]int64{}
	var errs []string

	// ── 1) user_id (ObjectID) ne bandhlele collections ───────────────────────
	byUserID := []string{
		"access",
		"sessions",
		"watch_history",
		"stream_nonces",
		"delivered_cache",
		"sent_lecture_msgs",
		"pending_msg_deletions",
		"auto_delivery_users",
		"auto_delivery_courses",
		"auto_delivery_items",
		"offline_tokens",
		"offline_daily_usage",
		"user_guard",
		"tg_login_tokens",
		"user_messages",
		"pending_course_sends",
	}
	for _, name := range byUserID {
		n, err := deleteManyIn(name, bson.M{"user_id": u.ID})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if n > 0 {
			counts[name] = n
		}
	}

	// ── 2) email / mobile / username ne bandhlele (login, OTP, registration) ──
	idents := identifierVariants(u)
	if len(idents) > 0 {
		byEmail := bson.M{"email": bson.M{"$in": emailVariants(u)}}
		for _, name := range []string{"otp_records", "otp_lockouts", "email_provider_map"} {
			if len(emailVariants(u)) == 0 {
				break
			}
			n, err := deleteManyIn(name, byEmail)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			if n > 0 {
				counts[name] = n
			}
		}

		n, err := deleteManyIn("login_attempts", bson.M{"identifier": bson.M{"$in": idents}})
		if err != nil {
			errs = append(errs, fmt.Sprintf("login_attempts: %v", err))
		} else if n > 0 {
			counts["login_attempts"] = n
		}

		// pending_registrations — email KIVA mobile match
		or := bson.A{}
		if ev := emailVariants(u); len(ev) > 0 {
			or = append(or, bson.M{"email": bson.M{"$in": ev}})
		}
		if u.Mobile != "" {
			or = append(or, bson.M{"mobile": u.Mobile})
		}
		if len(or) > 0 {
			n, err := deleteManyIn("pending_registrations", bson.M{"$or": or})
			if err != nil {
				errs = append(errs, fmt.Sprintf("pending_registrations: %v", err))
			} else if n > 0 {
				counts["pending_registrations"] = n
			}
		}
	}

	// ── 3) Telegram ID ne bandhlele ──────────────────────────────────────────
	if u.TelegramID > 0 {
		n, err := deleteManyIn("relay_map", bson.M{"user_telegram_id": u.TelegramID})
		if err != nil {
			errs = append(errs, fmt.Sprintf("relay_map: %v", err))
		} else if n > 0 {
			counts["relay_map"] = n
		}

		// /dl (offline download) che 48h auto-delete schedule records — chat_id ne bandhlele
		n, err = deleteManyIn("pending_deletes", bson.M{"chat_id": u.TelegramID})
		if err != nil {
			errs = append(errs, fmt.Sprintf("pending_deletes: %v", err))
		} else if n > 0 {
			counts["pending_deletes"] = n
		}
	}

	// ── 4) In-memory caches (access / delivered) ─────────────────────────────
	invalidateUserCaches(u.ID)

	// ── 5) Kahihi fail zala tar users doc THEVA — retry karta yeil ───────────
	if len(errs) > 0 {
		return counts, errors.New("kahi data delete zala nahi (retry kara): " + strings.Join(errs, "; "))
	}

	// ── 6) Shevti: users document ────────────────────────────────────────────
	ctx, cancel := Ctx()
	defer cancel()
	res, err := ColUsers.DeleteOne(ctx, bson.M{"_id": u.ID})
	if err != nil {
		return counts, err
	}
	counts["users"] = res.DeletedCount
	return counts, nil
}

// PendingDeletesForChat — ya Telegram chat madhle ajun delete na zalele (scheduled) bot messages.
// User delete karnyaadhi he messages chat madhun kadhayla vaparto.
func PendingDeletesForChat(chatID int64) ([]PendingDelete, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colPendingDeletes().Find(ctx, bson.M{"chat_id": chatID, "deleted": false})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []PendingDelete
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func deleteManyIn(collection string, filter bson.M) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()
	res, err := Database.Collection(collection).DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func emailVariants(u *User) []string {
	e := strings.TrimSpace(u.Email)
	if e == "" {
		return nil
	}
	l := strings.ToLower(e)
	if l == e {
		return []string{e}
	}
	return []string{e, l}
}

func identifierVariants(u *User) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, e := range emailVariants(u) {
		add(e)
	}
	add(u.Mobile)
	add(u.Username)
	return out
}

// ═══════════════════════════════════════════════════════════════════════════════
// PENDING COURSE SEND  — admin ne course "send" kela pan user ne ajun bot var
// msg kela nahi / Telegram link nahi. User link zala ki background worker
// (commands.StartPendingCourseSend) apoaap lectures pathavto.
// ═══════════════════════════════════════════════════════════════════════════════

type PendingCourseSend struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	CourseID  primitive.ObjectID `bson:"course_id"`
	Attempts  int                `bson:"attempts"`
	CreatedAt time.Time          `bson:"created_at"`
}

func colPendingCourseSends() *mongo.Collection {
	return Database.Collection("pending_course_sends")
}

// QueuePendingCourseSend — (user, course) pair ekach velach; punha call kela tar refresh.
func QueuePendingCourseSend(userID, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingCourseSends().UpdateOne(ctx,
		bson.M{"user_id": userID, "course_id": courseID},
		bson.M{
			"$set":         bson.M{"attempts": 0},
			"$setOnInsert": bson.M{"created_at": time.Now()},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func ListPendingCourseSends() ([]PendingCourseSend, error) {
	ctx, cancel := Ctx()
	defer cancel()
	cur, err := colPendingCourseSends().Find(ctx, bson.M{},
		options.Find().SetSort(bson.M{"created_at": 1}).SetLimit(200))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []PendingCourseSend
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func RemovePendingCourseSend(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colPendingCourseSends().DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func IncPendingCourseSendAttempt(id primitive.ObjectID) int {
	ctx, cancel := Ctx()
	defer cancel()
	var after PendingCourseSend
	err := colPendingCourseSends().FindOneAndUpdate(ctx,
		bson.M{"_id": id},
		bson.M{"$inc": bson.M{"attempts": 1}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&after)
	if err != nil {
		return 0
	}
	return after.Attempts
}

// HasPendingCourseSend — user chya navavar admin-queued send aahe ka?
// (auto-delivery popup donda-vegla duplicate pathavu naye mhanun check.)
func HasPendingCourseSend(userID primitive.ObjectID) bool {
	ctx, cancel := Ctx()
	defer cancel()
	n, err := colPendingCourseSends().CountDocuments(ctx, bson.M{"user_id": userID})
	return err == nil && n > 0
}
