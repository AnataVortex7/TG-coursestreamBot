package commands

// AutoSync — userbot session expire / revoke / logout handling.
//   - Dead session (AUTH_KEY_UNREGISTERED / SESSION_REVOKED / SESSION_EXPIRED / USER_DEACTIVATED[_BAN])
//     detect zala (ani ping pan fail) => DB madhla session delete + client stop.
//   - Task kharab hot nahi: cursor tasach rahto, session nasel tar scheduler task pause thevto
//     (spam nahi) ani admin/user la "🔑 Login" button sobat EKDACH notification jato.
//   - Login zalyavar task aapoaap parat chalto (cursor pasun).
import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"

	"github.com/gotd/td/tg"
)

var asDeadCodes = []string{
	"AUTH_KEY_UNREGISTERED",
	"SESSION_REVOKED",
	"SESSION_EXPIRED",
	"USER_DEACTIVATED",
}

// asIsDeadSessErr — Telegram ne session kayamcha marla (AUTH_KEY_DUPLICATED yat nahi — to retry-able).
func asIsDeadSessErr(err error) bool {
	if err == nil {
		return false
	}
	up := strings.ToUpper(err.Error())
	if strings.Contains(up, "AUTH_KEY_DUPLICATED") {
		return false
	}
	for _, c := range asDeadCodes {
		if strings.Contains(up, c) {
			return true
		}
	}
	return false
}

// asSessionExists — ya session key sathi login (DB / live client) aahe ka.
func asSessionExists(key string) bool {
	if strings.HasPrefix(key, "user_") {
		id, err := strconv.ParseInt(strings.TrimPrefix(key, "user_"), 10, 64)
		if err != nil || id == 0 {
			return false
		}
		d, _ := db.GetUserBotSessionDocForUser(id)
		return d != nil && d.Session != ""
	}
	if d, _ := db.GetUserBotSessionDoc(); d != nil && d.Session != "" {
		return true
	}
	if config.ValueOf.UserSession != "" {
		return true
	}
	if GetLiveUserbotClient != nil && GetLiveUserbotClient() != nil {
		return true
	}
	return false
}

// asDeleteSession — session DB madhun delete + client stop (logout sarkhe).
func asDeleteSession(key string) {
	if strings.HasPrefix(key, "user_") {
		if id, err := strconv.ParseInt(strings.TrimPrefix(key, "user_"), 10, 64); err == nil && id != 0 {
			_ = db.DeleteUserBotSessionForUser(id)
			dropUserbotClient(id)
			if du, _ := db.GetUserByTelegramID(id); du != nil {
				_ = db.SetUserTgApiCreds(du.ID, 0, "")
			}
		}
		return
	}
	if UserbotStop != nil {
		UserbotStop()
	}
	_ = db.DeleteUserBotSession()
	config.ValueOf.UserSession = ""
}

// asHandleDeadSession — err dead-session asel ANI userbot ping fail asel tar session delete karto (true).
func asHandleDeadSession(ctx context.Context, t *db.AutoSyncTask, err error, ping func(context.Context) bool) bool {
	if !asIsDeadSessErr(err) {
		return false
	}
	if ping != nil && ctx.Err() == nil && ping(ctx) {
		return false // client jivant — he error dusrya kashane (bot side) aala
	}
	asDeleteSession(t.SessionKey)
	return true
}

// ─── notification ────────────────────────────────────────────────────────────

var asDeadNotified sync.Map // session key -> time.Time

func asBtn(text, data string) tg.KeyboardButtonClass {
	return &tg.KeyboardButtonCallback{Text: text, Data: []byte(data)}
}

func asSendTo(chatID int64, text string, markup tg.ReplyMarkupClass) {
	if MainBotClient == nil || chatID == 0 {
		return
	}
	gctx := MainBotClient.CreateContext()
	_, _ = gctx.SendMessage(chatID, &tg.MessagesSendMessageRequest{Message: text, ReplyMarkup: markup})
}

// asNotifyLoginNeeded — session nahi / expire: login button sobat sandesh (6 tas madhe ekdach).
func asNotifyLoginNeeded(t *db.AutoSyncTask, reason string) {
	if v, ok := asDeadNotified.Load(t.SessionKey); ok {
		if time.Since(v.(time.Time)) < 6*time.Hour {
			return
		}
	}
	asDeadNotified.Store(t.SessionKey, time.Now())

	loginRow := tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{asBtn("🔑 Login Userbot", "wc:userbot_login")}}
	text := fmt.Sprintf("🔴 AutoSync “%s” — Userbot login nahi\n%s\n\n⏸ Task pause ahe (cursor surakshit — message harvat nahi). Login kelyavar aapoaap pudhe chalel.", t.Name, reason)

	if strings.HasPrefix(t.SessionKey, "user_") {
		if id, err := strconv.ParseInt(strings.TrimPrefix(t.SessionKey, "user_"), 10, 64); err == nil && id != 0 {
			asSendTo(id, text, &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{loginRow}})
		}
		adm := fmt.Sprintf("🔴 AutoSync “%s” — user %s cha Userbot session expire/logout.\nTyala 🔑 Login karayla sanga, kiva khali account list madhun session delete kara.", t.Name, strings.TrimPrefix(t.SessionKey, "user_"))
		for _, a := range config.ValueOf.AllowedUsers {
			asSendTo(a, adm, &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{asBtn("👥 Userbot Accounts", "wc:userbot_select_user")}},
			}})
		}
		return
	}
	for _, a := range config.ValueOf.AllowedUsers {
		asSendTo(a, text, &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
			loginRow,
			{Buttons: []tg.KeyboardButtonClass{asBtn("👥 Userbot Accounts", "wc:userbot_select_user")}},
		}})
	}
}

// asClearLoginNotified — login zalyavar parat notification chalu hoil.
func asClearLoginNotified(key string) { asDeadNotified.Delete(key) }

// ─── admin: dusrya account cha logout ────────────────────────────────────────

// handleAdminUserbotLogout — callback "ub_logout_<ownerID>" (confirm) / "ub_logoutyes_<ownerID>" (execute).
// ownerID 0 = Admin System Userbot. Fakt admin.
func handleAdminUserbotLogout(adminID int64, data string) (string, tg.ReplyMarkupClass) {
	isYes := strings.HasPrefix(data, "ub_logoutyes_")
	idStr := strings.TrimPrefix(strings.TrimPrefix(data, "ub_logoutyes_"), "ub_logout_")
	ownerID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || ownerID < 0 {
		return renderImpersonateMenu(adminID)
	}
	label := fmt.Sprintf("User %d", ownerID)
	var doc *db.UserBotSessionDoc
	if ownerID == 0 {
		label = "Admin System Userbot (Default)"
		doc, _ = db.GetUserBotSessionDoc()
	} else {
		doc, _ = db.GetUserBotSessionDocForUser(ownerID)
	}
	if doc != nil {
		n := doc.FirstName
		if n == "" {
			n = doc.Phone
		}
		label = fmt.Sprintf("%s — %s", label, n)
	}

	if !isYes {
		return fmt.Sprintf("⚠️ *Logout confirm*\n\n%s\n\nHa session delete hoil ani hyavar chalnare AutoSync tasks pause hotil (login kelyavar parat chalatil). Pudhe jaaycha?", label),
			&tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					asBtn("✅ Ho, Logout", fmt.Sprintf("wc:ub_logoutyes_%d", ownerID)),
					asBtn("❌ Cancel", "wc:userbot_select_user"),
				}},
			}}
	}

	key := "main"
	if ownerID != 0 {
		key = "user_" + strconv.FormatInt(ownerID, 10)
	}
	asDeleteSession(key)
	AdminImpersonateStore.Range(func(k, v any) bool {
		if tid, ok := v.(int64); ok && tid == ownerID {
			AdminImpersonateStore.Delete(k)
		}
		return true
	})
	text, markup := renderImpersonateMenu(adminID)
	return fmt.Sprintf("🚪 Logout zala — %s (session delete).\n\n", label) + text, markup
}
