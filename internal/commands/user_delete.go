// internal/commands/user_delete.go
//
// User PERMANENT delete — ekach function, don thikani vaparla jato:
//   - Admin panel (DELETE /api/admin/users/:id)
//   - Telegram bot admin command / menu "🗑 Delete User" (/deluser)
package commands

import (
	"errors"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// UserDeleteResult — delete cha summary.
type UserDeleteResult struct {
	DB              map[string]int64
	TelegramDeleted int
	TelegramFailed  int
}

// DeleteUserPermanently — 1) bot-sent Telegram messages kadhto (best-effort, 25s cap)
// 2) DB madhun sarva data kayamcha delete 3) bot cha in-memory state pusto.
func DeleteUserPermanently(user *db.User) (*UserDeleteResult, error) {
	if user == nil {
		return nil, errors.New("user nil")
	}
	if db.IsProtectedUserDoc(user.ID) ||
		(user.TelegramID != 0 && utils.Contains(config.ValueOf.AllowedUsers, user.TelegramID)) {
		return nil, db.ErrProtectedUser
	}

	res := &UserDeleteResult{}

	if user.TelegramID != 0 && MainBotClient != nil && MainBotClient.Self != nil {
		type clr struct{ deleted, failed int }
		ch := make(chan clr, 1)
		go func() {
			d, f, _ := ClearTrackedMsgs(MainBotClient, user.ID, primitive.NilObjectID, utils.Logger)
			d += ClearScheduledChatMsgs(MainBotClient, user.TelegramID)
			ch <- clr{d, f}
		}()
		select {
		case r := <-ch:
			res.TelegramDeleted, res.TelegramFailed = r.deleted, r.failed
		case <-time.After(25 * time.Second):
			utils.Logger.Sugar().Warnw("delete user: Telegram cleanup timeout — DB purge pudhe chalu", "user", user.ID.Hex())
		}
	}

	counts, err := db.PurgeUserCompletely(user)
	if err != nil {
		return nil, err
	}
	res.DB = counts

	if user.TelegramID != 0 {
		ForgetUserBotState(user.TelegramID)
	}
	utils.Logger.Sugar().Infow("user permanently deleted", "user", user.ID.Hex(), "db", counts,
		"tg_deleted", res.TelegramDeleted, "tg_failed", res.TelegramFailed)
	return res, nil
}
