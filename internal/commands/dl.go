// internal/commands/dl.go
// /dl command REMOVED — deep link flow वापरा:
// Website → 📥 Offline → https://t.me/botname?start=dl_TOKEN → direct video
//
// हा file फक्त admin commands आणि helper functions ठेवतो.

package commands

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

// LoadDL — फक्त /settg admin command register करतो (/dl काढून टाकला)
func (m *command) LoadDL(d dispatcher.Dispatcher) {
	log := m.log.Named("dl")
	defer log.Sugar().Info("Loaded /settg command")
	d.AddHandler(handlers.NewCommand("settg", adminOnly(cmdSetTG)))
}

// ── /settg <identifier> <telegram_id> ────────────────────────────────────────
// Admin command: website user चा DB record Telegram ID शी link करतो.
// Usage: /settg 9876543210 987654321
//        /settg user@email.com 987654321

func cmdSetTG(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"Usage: `/settg <mobile_or_email> <telegram_id>`\n\n"+
				"Example:\n`/settg 9876543210 987654321`\n`/settg user@email.com 987654321`\n\n"+
				"User चा Telegram ID बघायला: `/myid` bot मध्ये.")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))

	var tgID int64
	if _, err := fmt.Sscanf(a[1], "%d", &tgID); err != nil || tgID < 1 {
		reply(ctx, u, "❌ Invalid Telegram ID.")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	if err := db.SetUserTelegramID(user.ID, tgID); err != nil {
		reply(ctx, u, "❌ DB update error: "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Linked!\n👤 User: %s\n📱 Telegram ID: %d\n\nआता user website वरून direct video मिळवू शकतो.",
		user.Username, tgID,
	))
	return dispatcher.EndGroups
}

// ── /myid ─────────────────────────────────────────────────────────────────────

func (m *command) LoadMyID(d dispatcher.Dispatcher) {
	d.AddHandler(handlers.NewCommand("myid", func(ctx *ext.Context, u *ext.Update) error {
		chatID := u.EffectiveChat().GetID()
		reply(ctx, u, fmt.Sprintf("तुमचा Telegram ID:\n`%d`\n\nहा ID admin ला द्या /settg साठी.", chatID))
		return dispatcher.EndGroups
	}))
}

// ── Helper functions ──────────────────────────────────────────────────────────

func istLoc() *time.Location {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		return time.FixedZone("IST", 5*60*60+30*60)
	}
	return loc
}

func getAdminIDs() []int64 {
	if MainBotClient == nil {
		return nil
	}
	return []int64{MainBotClient.Self.ID}
}

func sendAdminAlert(ctx *ext.Context, user *db.User, score int) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	usedToday, _ := db.GetDailyUsageCount(user.ID)
	guard, _ := db.GetGuard(user.ID)
	consecutive := 0
	if guard != nil {
		consecutive = guard.ConsecutiveFullDays
	}

	identifier := user.Mobile
	if identifier == "" {
		identifier = user.Email
	}

	msg := fmt.Sprintf(
		"🚨 *Suspicious Activity Alert*\n\n"+
			"👤 User: `%s`\n"+
			"📱 Mobile: %s\n"+
			"📧 Email: %s\n"+
			"📊 Score: *%d*/15\n"+
			"📅 Consecutive Days: %d\n"+
			"⬇️ Downloads Today: %d/3\n\n"+
			"Block करायला:\n`/blockuser %s 10 Suspicious activity`",
		user.Username, user.Mobile, user.Email,
		score, consecutive, usedToday, identifier,
	)

	adminIDs := getAdminIDs()
	for _, adminID := range adminIDs {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  msg,
			RandomID: rand.Int63(),
		})
	}
}
