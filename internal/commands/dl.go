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

	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

// LoadDL — /settg आणि /unlinktg admin commands register करतो (/dl काढून टाकला)
func (m *command) LoadDL(d dispatcher.Dispatcher) {
	log := m.log.Named("dl")
	defer log.Sugar().Info("Loaded /settg, /unlinktg commands")
	d.AddHandler(handlers.NewCommand("settg", adminOnly(cmdSetTG)))
	d.AddHandler(handlers.NewCommand("unlinktg", adminOnly(cmdUnlinkTG)))
}

// ── /settg <identifier> <telegram_id> ────────────────────────────────────────
// Admin command: website user चा DB record Telegram ID शी link करतो.
// One user ↔ one Telegram ID, कायमचं — आधीच link असेल तर हा command काम करणार नाही
// (आधी /unlinktg वापरावं लागेल).
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
		switch err {
		case db.ErrTelegramAlreadyLinked:
			reply(ctx, u, fmt.Sprintf(
				"⚠️ हा account (%s) आधीच एका Telegram ID शी link आहे.\n\n"+
					"आधी unlink करा: `/unlinktg %s`",
				identifier, identifier,
			))
		case db.ErrTelegramIDInUse:
			reply(ctx, u, "❌ हा Telegram ID आधीच दुसऱ्या website account शी link आहे.\n\nएक Telegram account फक्त एकाच user शी link होऊ शकतो.")
		default:
			reply(ctx, u, "❌ DB update error: "+err.Error())
		}
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Linked!\n👤 User: %s\n📱 Telegram ID: %d\n\nआता user Telegram वरून OTP login वापरू शकतो.",
		user.Username, tgID,
	))

	// जर या user ला आधी pre-download cache केलेले courses असतील तर admin ला
	// notify करा — /predownload परत चालवल्यावर fast cache-hit delivery होईल.
	go func() {
		accesses, err := db.ActiveAccessForUser(user.ID)
		if err != nil || len(accesses) == 0 {
			return
		}
		var courseNames []string
		for _, acc := range accesses {
			if c, e := db.GetCourseByID(acc.CourseID); e == nil && c != nil {
				courseNames = append(courseNames, "• "+c.Title)
			}
		}
		if len(courseNames) == 0 {
			return
		}
		msg := fmt.Sprintf(
			"🔗 *TG Linked!*\n👤 User: `%s` (mobile: `%s`)\n📱 TG ID: `%d`\n\n📚 *%d course access आहे:*\n%s\n\n⚡ Pre-download cache असेल तर `/predownload %s <courseID>` चालवा — fast होईल.",
			user.FullName, user.Mobile, tgID,
			len(courseNames), strings.Join(courseNames, "\n"),
			user.Mobile,
		)
		bot.NotifyAllowedUsers(msg)
	}()

	return dispatcher.EndGroups
}

// ── /unlinktg <identifier> ───────────────────────────────────────────────────
// Admin-only emergency unlink. Normal flow मध्ये कधीच वापरला जात नाही — फक्त
// support साठी (उदा. user चा phone हरवला / Telegram account delete झाला).
// Unlink झाल्यावर तो account परत fresh unlinked सारखा — user स्वतः deep-link ने
// किंवा admin परत /settg ने, दोन्हीपैकी कुठल्याही मार्गाने relink करू शकतो.
// Usage: /unlinktg 9876543210
//        /unlinktg user@email.com

func cmdUnlinkTG(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"Usage: `/unlinktg <mobile_or_email>`\n\n"+
				"⚠️ फक्त emergency/support साठी वापरा (user चा phone हरवला, Telegram account delete झाला, इ.)\n"+
				"Unlink झाल्यावर user परत deep-link किंवा /settg ने relink करू शकतो.")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	oldTgID := user.TelegramID

	if err := db.UnlinkUserTelegramID(user.ID); err != nil {
		if err == db.ErrTelegramNotLinked {
			reply(ctx, u, "⚠️ हा account आधीच unlinked आहे.")
		} else {
			reply(ctx, u, "❌ Unlink करताना error: "+err.Error())
		}
		return dispatcher.EndGroups
	}

	// Audit — कोणी, कधी, कोणासाठी unlink केलं याचा admin log (misuse ट्रॅक करायला)
	adminName := "admin"
	if senderUser := u.EffectiveUser(); senderUser != nil {
		adminName = senderUser.FirstName
	}
	logMsg := fmt.Sprintf(
		"🔓 *Telegram Unlinked (Emergency)*\n\n"+
			"👤 User: %s\n"+
			"📱 जुना Telegram ID: %d\n"+
			"🛠 केलं: %s\n\n"+
			"आता user deep-link किंवा /settg ने परत link करू शकतो.",
		identifier, oldTgID, adminName,
	)
	for _, adminID := range getAdminIDs() {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = MainBotClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  logMsg,
			RandomID: rand.Int63(),
		})
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Unlinked!\n👤 User: %s\n📱 जुना Telegram ID: %d\n\nआता हा account पुन्हा link करता येईल.",
		user.Username, oldTgID,
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

	usedToday, _ := db.GetTotalDailyUsageCount(user.ID)
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
