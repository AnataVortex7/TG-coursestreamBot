// internal/commands/guard.go
//
// Admin commands:
//   /blockuser <mobile_or_email> <days> [reason]
//   /unblockuser <mobile_or_email>
//   /suspicious  — score >= 15 असलेले सगळे users
//   /userinfo <mobile_or_email> — एका user चा पूर्ण guard status

package commands

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
)

func (m *command) LoadGuard(d dispatcher.Dispatcher) {
	log := m.log.Named("guard")
	defer log.Sugar().Info("Loaded guard commands")

	d.AddHandler(handlers.NewCommand("blockuser",   adminOnly(cmdBlockUser)))
	d.AddHandler(handlers.NewCommand("unblockuser", adminOnly(cmdUnblockUser)))
	d.AddHandler(handlers.NewCommand("suspicious",  adminOnly(cmdSuspicious)))
	d.AddHandler(handlers.NewCommand("userinfo",    adminOnly(cmdUserInfo)))
}

// ── /blockuser <mobile_or_email> <days> [reason] ─────────────────────────────

func cmdBlockUser(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"Usage: `/blockuser <mobile_or_email> <days> [reason]`\n\n"+
				"Example:\n`/blockuser 9876543210 10 Suspicious downloads`")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))
	days, err := strconv.Atoi(a[1])
	if err != nil || days < 1 {
		reply(ctx, u, "❌ Days valid number असणे आवश्यक आहे (minimum 1).")
		return dispatcher.EndGroups
	}

	reason := "Admin ने block केले"
	if len(a) >= 3 {
		reason = strings.Join(a[2:], " ")
	}

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	if err := db.BlockUser(user.ID, days, reason); err != nil {
		reply(ctx, u, "❌ DB error: "+err.Error())
		return dispatcher.EndGroups
	}

	until := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	reply(ctx, u, fmt.Sprintf(
		"🔴 Blocked!\n\n"+
			"👤 User: %s\n"+
			"⏳ Duration: %d days\n"+
			"📅 Until: %s IST\n"+
			"📝 Reason: %s",
		user.Username,
		days,
		until.In(istLoc()).Format("02 Jan 2006, 3:04 PM"),
		reason,
	))
	return dispatcher.EndGroups
}

// ── /unblockuser <mobile_or_email> ───────────────────────────────────────────

func cmdUnblockUser(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: `/unblockuser <mobile_or_email>`")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	if err := db.UnblockUser(user.ID); err != nil {
		reply(ctx, u, "❌ DB error: "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf("✅ %s ला unblock केले.", user.Username))
	return dispatcher.EndGroups
}

// ── /suspicious ───────────────────────────────────────────────────────────────

func cmdSuspicious(ctx *ext.Context, u *ext.Update) error {
	guards, err := db.GetSuspiciousUsers()
	if err != nil {
		reply(ctx, u, "❌ DB error: "+err.Error())
		return dispatcher.EndGroups
	}

	if len(guards) == 0 {
		reply(ctx, u, "✅ कोणतेही suspicious users नाहीत.")
		return dispatcher.EndGroups
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚠️ *Suspicious Users* (%d)\n\n", len(guards)))

	for i, g := range guards {
		user, _ := db.GetUserByID(g.UserID)
		name := g.UserID.Hex()[:8] + "..."
		if user != nil {
			name = user.Username
		}

		status := "🟢 Active"
		if g.IsBlocked {
			status = fmt.Sprintf("🔴 Blocked until %s",
				g.BlockedUntil.In(istLoc()).Format("02 Jan"))
		}

		sb.WriteString(fmt.Sprintf(
			"%d. `%s`\n"+
				"   Score: %d | Consecutive: %d days\n"+
				"   %s\n\n",
			i+1, name, g.SuspicionScore, g.ConsecutiveFullDays, status,
		))
	}

	sb.WriteString("Block करण्यासाठी:\n`/blockuser <mobile> <days>`")
	reply(ctx, u, sb.String())
	return dispatcher.EndGroups
}

// ── /userinfo <mobile_or_email> ──────────────────────────────────────────────

func cmdUserInfo(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: `/userinfo <mobile_or_email>`")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	g, err := db.GetGuard(user.ID)
	if err != nil {
		reply(ctx, u, "❌ DB error: "+err.Error())
		return dispatcher.EndGroups
	}

	usedToday, _ := db.GetDailyUsageCount(user.ID)

	status := "🟢 Active"
	if g != nil && g.IsBlocked {
		status = fmt.Sprintf("🔴 Blocked until %s IST",
			g.BlockedUntil.In(istLoc()).Format("02 Jan 2006, 3:04 PM"))
	}

	score := 0
	consecutive := 0
	if g != nil {
		score = g.SuspicionScore
		consecutive = g.ConsecutiveFullDays
	}

	tgLink := "Not linked"
	if user.TelegramID != 0 {
		tgLink = fmt.Sprintf("`%d`", user.TelegramID)
	}

	reply(ctx, u, fmt.Sprintf(
		"👤 *User Info*\n\n"+
			"Name: %s\n"+
			"Mobile: %s\n"+
			"Email: %s\n"+
			"TG ID: %s\n"+
			"Status: %s\n\n"+
			"📊 *Guard Stats*\n"+
			"Suspicion Score: %d / 15\n"+
			"Consecutive Full Days: %d\n"+
			"Downloads Today: %d / 5\n",
		user.FullName, user.Mobile, user.Email, tgLink,
		status, score, consecutive, usedToday,
	))
	return dispatcher.EndGroups
}



