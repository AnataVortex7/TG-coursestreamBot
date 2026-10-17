// internal/commands/send_course.go
//
// Admin → user च्या Telegram chat मध्ये course चे lectures थेट पाठवतो
// (user ने स्वतः मागितल्याशिवाय). Admin panel cha "Send Course" sarkhach logic (SendCourseRange):
// watermarked copy cache madhe asel tar tich, nasel tar original lagech ani nantar auto-replace;
// user ne ajun bot var msg kela nasel tar queue.
package commands

import (
	"fmt"
	"strconv"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/ext"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// /sendcourse <mobile_or_email_or_telegramID> <courseID> [from] [to]
// from/to — lecture क्रमांक (1 पासून, inclusive). दिले नाहीत तर सगळे lectures.
func cmdSendCourse(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"📤 *Usage:*\n\n"+
				"`/sendcourse <mobile_or_email_or_telegramID> <courseID> [from] [to]`\n\n"+
				"*Examples:*\n"+
				"`/sendcourse 9876543210 64f1a2b3c4d5e6f7a8b9c0d1` → सगळे lectures\n"+
				"`/sendcourse 9876543210 64f1a2b3c4d5e6f7a8b9c0d1 1 5` → lecture 1 ते 5\n\n"+
				"ℹ️ User kade course access asava. User ne ajun bot var msg kela nasel tar send queue hoto ani to msg takun verify zala ki apoaap jato.")
		return dispatcher.EndGroups
	}

	identifier := a[0]
	courseID, cErr := primitive.ObjectIDFromHex(a[1])
	if cErr != nil {
		reply(ctx, u, "❌ Invalid courseID.")
		return dispatcher.EndGroups
	}

	targetUser, uErr := db.GetUserByMobileOrEmail(identifier)
	if uErr != nil || targetUser == nil {
		if tgID, pErr := strconv.ParseInt(identifier, 10, 64); pErr == nil {
			targetUser, uErr = db.GetUserByTelegramID(tgID)
		}
	}
	if uErr != nil || targetUser == nil {
		reply(ctx, u, fmt.Sprintf("❌ User सापडला नाही: `%s`", identifier))
		return dispatcher.EndGroups
	}
	// Range (1-based, inclusive) — optional
	from, to := 0, 0
	if len(a) >= 3 {
		n, e := strconv.Atoi(a[2])
		if e != nil || n < 1 {
			reply(ctx, u, "❌ `from` 1 pasun suru honara number hava.")
			return dispatcher.EndGroups
		}
		from = n
	}
	if len(a) >= 4 {
		n, e := strconv.Atoi(a[3])
		if e != nil || n < 1 {
			reply(ctx, u, "❌ `to` valid number hava.")
			return dispatcher.EndGroups
		}
		to = n
	}

	// Admin panel sarkhach logic: access check, raw-first (watermark nantar replace),
	// ani user ne ajun bot var msg kela nasel tar queue — user link/msg zalyavar apoaap jail.
	res, sErr := SendCourseRange(targetUser.ID, courseID, from, to)
	if sErr != nil {
		reply(ctx, u, "❌ "+sErr.Error())
		return dispatcher.EndGroups
	}
	if res.Queued {
		reply(ctx, u, fmt.Sprintf(
			"⏳ `%s` ne ajun bot var msg/verification kela nahi.\n"+
				"📦 %d lectures queue madhe — user ne bot la msg takun number share kela ki apoaap jatil.",
			targetUser.FullName, res.Lectures))
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf(
		"📤 Pathavna survat zala.\n👤 `%s`\n📦 Lectures: %d\n\nShevti summary yeil. Watermarked copy tayar nasel tar aadhi original jail, nantar apoaap replace hoil.",
		targetUser.FullName, res.Lectures))
	return dispatcher.EndGroups
}
