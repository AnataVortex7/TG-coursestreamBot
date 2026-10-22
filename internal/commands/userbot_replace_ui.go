// internal/commands/userbot_replace_ui.go
//
// "⭐ Userbot Settings" मेन्यूमधले Replace बटण्स (Set Replace Rule / Remove Replace Word).
//   - Userbot user  → rules त्याच्या स्वतःच्या DB doc मध्ये (प्रत्येक user चे वेगळे).
//   - Admin         → rules GLOBAL (सगळ्या users साठी लागू).
// Admin ने /setreplace किंवा या बटणातून add केलेले global rules प्रत्येक user च्या
// captions ना आपोआप आधी लागू होतात (applyReplacesForUser).
package commands

import (
	"fmt"
	"strconv"
	"strings"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

const maxReplaceButtons = 30

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func shortRuleLabel(r db.ReplaceRule) string {
	n := strings.Join(strings.Fields(r.New), " ")
	if n == "" {
		n = "(काढा)"
	}
	old := strings.Join(strings.Fields(r.Old), " ")
	label := fmt.Sprintf("%s → %s", clipRunes(old, 14), clipRunes(n, 14))
	if r.Line {
		label = "[line] " + label
	}
	return label
}

// renderReplaceManageView — rules ची यादी + प्रत्येकासाठी 🗑 बटण + Add / Clear All.
func renderReplaceManageView(userID int64) (string, tg.ReplyMarkupClass) {
	admin := isReplaceAdmin(userID)
	rules := loadRulesFor(userID)

	var sb strings.Builder
	if admin {
		sb.WriteString("🔁 Replace Rules — 🌐 सगळ्यांसाठी (Global)\n\n")
	} else {
		sb.WriteString("🔁 तुमचे Replace Rules\n\n")
	}
	if len(rules) == 0 {
		sb.WriteString("(एकही rule नाही)\n")
	}
	for i, r := range rules {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, ruleDisplay(r)))
	}
	if !admin {
		if g := len(globalReplaceRules()); g > 0 {
			sb.WriteString(fmt.Sprintf("\n🌐 Admin चे %d global rules सगळ्यांना आपोआप लागू असतात.\n", g))
		}
	}
	if len(rules) > 0 {
		sb.WriteString("\nrule काढायला खालचं 🗑 बटण दाबा.")
	}

	var rows []tg.KeyboardButtonRow
	for i, r := range rules {
		if i >= maxReplaceButtons {
			break
		}
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{
				Text: fmt.Sprintf("🗑 %d. %s", i+1, shortRuleLabel(r)),
				Data: []byte(fmt.Sprintf("wc:userbot_replace_rm:%d", i+1)),
			},
		}})
	}
	actions := []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "➕ Add Rule", Data: []byte("wc:userbot_replace_add")},
	}
	if len(rules) > 0 {
		actions = append(actions, &tg.KeyboardButtonCallback{Text: "🧹 Clear All", Data: []byte("wc:userbot_replace_clear")})
	}
	rows = append(rows,
		tg.KeyboardButtonRow{Buttons: actions},
		tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "⬅️ Back to Userbot Menu", Data: []byte("wc:premium_menu")},
		}},
	)
	return sb.String(), &tg.ReplyInlineMarkup{Rows: rows}
}

func replaceAccessDenied(ctx *ext.Context, u *ext.Update, userID int64) bool {
	if db.HasUserbotAccess(userID) {
		return false
	}
	editCatalogMessage(ctx, u, "⛔ तुम्हाला Userbot Features चा ॲक्सेस नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
	return true
}

func setPendingReplacePrompt(ctx *ext.Context, u *ext.Update, userID int64) {
	scope := "हे rule फक्त तुमच्या downloads ना लागू होईल."
	if isReplaceAdmin(userID) {
		scope = "🌐 Admin म्हणून add केलेला rule सगळ्या users ना लागू होईल."
	}
	prompt := "🔁 Set Replace Rule\n\n" +
		"`old => new` (किंवा `old=new`) असं पाठवा. एकापेक्षा जास्त rules हवे तर प्रत्येक ओळीत एक.\n\n" +
		"Examples:\n" +
		"`DOWNLOAD by ROCKSTAR 𓆩𓂍𓆪=𝐆𝐄𝐓 𝐈𝐓 𝐇𝐄𝐑𝐄 𝐛𝐲 𓆩𓂍𓆪`\n" +
		"`hello=नमस्ते` (एकच शब्द)\n" +
		"`@oldchannel =>` (new रिकामा = काढून टाका)\n" +
		"`line: ROCKSTAR => 📚 My Course` (ROCKSTAR असलेली पूर्ण ओळ)\n\n" +
		"✅ Fancy fonts, CAPS/small, जास्त spaces, symbols, मराठी — सगळं match होतं.\n" + scope
	setPending(userID, &pendingAction{
		key:         "setreplace_user",
		currentStep: inputStep(prompt),
	})
	editCatalogMessage(ctx, u, prompt, &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:userbot_replace_list")})
}

// handleSetReplaceReply — user ने पाठवलेल्या text मधून (प्रत्येक ओळ = एक rule) rules save करतो.
func handleSetReplaceReply(ctx *ext.Context, u *ext.Update, userID int64, text string) {
	added, updated := 0, 0
	var errs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rule, err := parseReplaceSpec(line)
		if err != nil {
			errs = append(errs, fmt.Sprintf("❌ %s — %s", clipRunes(line, 30), err.Error()))
			continue
		}
		upd, _, serr := addReplaceRuleFor(userID, rule)
		if serr != nil {
			errs = append(errs, fmt.Sprintf("❌ %s — save failed: %s", clipRunes(line, 30), serr.Error()))
			continue
		}
		if upd {
			updated++
		} else {
			added++
		}
	}
	summary := fmt.Sprintf("✅ %d नवीन rule, %d update झाले.", added, updated)
	if added+updated == 0 {
		summary = "❌ एकही rule save झाला नाही. Format: `old => new`"
	}
	if added+updated > 0 && isReplaceAdmin(userID) {
		summary += " (🌐 सगळ्यांसाठी लागू)"
	}
	if len(errs) > 0 {
		summary += "\n\n" + strings.Join(errs, "\n")
	}
	mtext, markup := renderReplaceManageView(userID)
	_, _ = ctx.Reply(u, ext.ReplyTextString(summary+"\n\n"+mtext), &ext.ReplyOpts{Markup: markup})
}

// handleReplaceCallback — wc:userbot_replace_* callbacks. true = handled.
func handleReplaceCallback(ctx *ext.Context, u *ext.Update, chatID int64, parts []string) bool {
	switch parts[0] {
	case "userbot_replace_add":
		if replaceAccessDenied(ctx, u, chatID) {
			return true
		}
		setPendingReplacePrompt(ctx, u, chatID)
	case "userbot_replace_list":
		if replaceAccessDenied(ctx, u, chatID) {
			return true
		}
		clearPending(chatID)
		text, markup := renderReplaceManageView(chatID)
		editCatalogMessage(ctx, u, text, markup)
	case "userbot_replace_rm":
		if replaceAccessDenied(ctx, u, chatID) {
			return true
		}
		head := ""
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				if removed, derr := deleteReplaceRuleAt(chatID, n-1); derr == nil {
					head = "✅ काढला: " + ruleDisplay(removed) + "\n\n"
				} else {
					head = "❌ Rule सापडला नाही (यादी बदललीय).\n\n"
				}
			}
		}
		text, markup := renderReplaceManageView(chatID)
		editCatalogMessage(ctx, u, head+text, markup)
	case "userbot_replace_clear":
		if replaceAccessDenied(ctx, u, chatID) {
			return true
		}
		n := len(loadRulesFor(chatID))
		_ = saveRulesFor(chatID, []db.ReplaceRule{})
		text, markup := renderReplaceManageView(chatID)
		editCatalogMessage(ctx, u, fmt.Sprintf("🧹 %d rules काढले.\n\n", n)+text, markup)
	default:
		return false
	}
	return true
}
