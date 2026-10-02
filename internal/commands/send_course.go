// internal/commands/send_course.go
//
// Admin → user च्या Telegram chat मध्ये course चे lectures थेट पाठवतो
// (user ने स्वतः मागितल्याशिवाय). हे /predownload सारखं cache-only नाही —
// silent=false, म्हणजे lecture user ला watermark सहित प्रत्यक्ष मिळतो.
package commands

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
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
				"⚠️ User चा Telegram link आणि course access दोन्ही हवेत — नाहीतर expiry cleanup हे lectures लगेच काढून टाकेल.")
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
	if targetUser.TelegramID == 0 {
		reply(ctx, u, "❌ या user चं Telegram account link नाहीये — आधी /settg करा.")
		return dispatcher.EndGroups
	}
	if hasAccess, _ := db.HasAccess(targetUser.ID, courseID); !hasAccess {
		reply(ctx, u, "❌ या user कडे या course चा access नाहीये — आधी Grant Access करा.")
		return dispatcher.EndGroups
	}
	course, courseErr := db.GetCourseByID(courseID)
	if courseErr != nil || course == nil {
		reply(ctx, u, "❌ Course सापडला नाही.")
		return dispatcher.EndGroups
	}
	files, fErr := db.FilesForCourse(courseID)
	if fErr != nil {
		reply(ctx, u, "❌ Error: "+fErr.Error())
		return dispatcher.EndGroups
	}
	if len(files) == 0 {
		reply(ctx, u, "ℹ️ या course मध्ये काहीच lectures नाहीत.")
		return dispatcher.EndGroups
	}

	// Range (1-based, inclusive)
	from, to := 1, len(files)
	if len(a) >= 3 {
		n, e := strconv.Atoi(a[2])
		if e != nil || n < 1 || n > len(files) {
			reply(ctx, u, fmt.Sprintf("❌ `from` 1 ते %d दरम्यान हवा.", len(files)))
			return dispatcher.EndGroups
		}
		from = n
	}
	if len(a) >= 4 {
		n, e := strconv.Atoi(a[3])
		if e != nil || n < from || n > len(files) {
			reply(ctx, u, fmt.Sprintf("❌ `to` %d ते %d दरम्यान हवा.", from, len(files)))
			return dispatcher.EndGroups
		}
		to = n
	}
	msgIDs := make([]int, 0, to-from+1)
	for _, f := range files[from-1 : to] {
		msgIDs = append(msgIDs, f.MessageID)
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही.")
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"📤 पाठवणं सुरू...\n👤 `%s`\n📚 `%s`\n📦 Lectures: %d (क्रमांक %d–%d)\n\nशेवटी summary येईल.",
		targetUser.FullName, course.Title, len(msgIDs), from, to))

	adminID := u.EffectiveUser().GetID()
	go func() {
		bg := context.Background()
		sent, failed := 0, 0
		for _, id := range msgIDs {
			ok, dErr := runWithFloodRetryGeneric(bg, 5, func() (bool, error) {
				return deliverLectureCore(ctx, botClient, targetUser, course, courseID,
					id, targetUser.TelegramID, 0, false, func(string) {})
			})
			if dErr != nil || !ok {
				failed++
			} else {
				sent++
			}
			time.Sleep(1500 * time.Millisecond)
		}

		msg := fmt.Sprintf("✅ `%s` ला %d lectures पाठवले.", targetUser.FullName, sent)
		if failed > 0 {
			msg += fmt.Sprintf("\n⚠️ %d अयशस्वी — परत /sendcourse चालवा (cache मुळे आधीचे लगेच जातील).", failed)
		}
		adminPeer := botClient.PeerStorage.GetInputPeerById(adminID)
		if adminPeer.Zero() {
			return
		}
		_, _ = botClient.API().MessagesSendMessage(bg, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  msg,
			RandomID: rand.Int63(),
		})
	}()

	return dispatcher.EndGroups
}
