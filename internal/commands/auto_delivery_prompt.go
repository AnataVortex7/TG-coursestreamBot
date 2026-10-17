// internal/commands/auto_delivery_prompt.go
//
// Auto-delivery popup: registered user ne /start kela ani ajun bot madhe kahi lecture
// aalele nahit tar vichartat: "sarv courses 1 pasun shevtparyant pathavu ka?"
// Confirm zalyavarch delivery suru hote (spam nahi). Summary message yeparyant thamba.
package commands

import (
	"fmt"
	"strings"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

const adRePromptAfter = 6 * time.Hour

func adPromptMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "✅ हो, सगळे पाठवा", Data: []byte("wc:adyes")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "❌ नको, मी स्वतः निवडतो", Data: []byte("wc:adno")},
		}},
	}}
}

// MaybePromptAutoDelivery -- /start (kiva link) nantar. Fakt yogya user la ani fakt yogya veli.
func MaybePromptAutoDelivery(ctx *ext.Context, chatID int64) {
	if !config.ValueOf.AutoDelivery || MainBotClient == nil || MainBotClient.Self == nil {
		return
	}
	user, err := db.GetUserByTelegramID(chatID)
	if err != nil || user == nil {
		return // registered nahi
	}
	if blocked, _, _ := db.IsUserBlocked(user.ID); blocked {
		return
	}
	if db.HasPendingCourseSend(user.ID) {
		return // admin ne course send queue kela aahe -- worker swatach pathavel, popup nako (duplicate tala)
	}
	accesses, err := db.ActiveAccessForUser(user.ID)
	if err != nil || len(accesses) == 0 {
		return // pathavayla kahi nahi
	}
	if ad := db.GetADUser(user.ID); ad != nil {
		if ad.Confirmed || ad.Declined {
			return // aadhi uttar dila
		}
		if time.Since(ad.PromptedAt) < adRePromptAfter {
			return // nukatach vichaarla -- spam nako
		}
	}
	if db.UserHasSentLectures(user.ID, MainBotClient.Self.ID) {
		return // lectures aadhich aalele aahet
	}

	names := make([]string, 0, len(accesses))
	for _, a := range accesses {
		if c, _ := db.GetCourseByID(a.CourseID); c != nil {
			names = append(names, "• "+c.Title)
		}
		if len(names) >= 8 {
			names = append(names, "• ...")
			break
		}
	}
	text := fmt.Sprintf("📚 तुमच्याकडे %d course(s) आहेत:\n%s\n\n"+
		"सगळे courses (1ल्या lecture पासून शेवटपर्यंत) folder-wise इथे आपोआप पाठवू का?\n\n"+
		"ℹ️ ज्या lectures ची तयारी अजून झालेली नाही त्यांच्या जागी ⏳ placeholder येईल आणि ते तयार झाल्यावर त्याच जागी आपोआप बदलेल.\n"+
		"⏳ पाठवणं सुरू झाल्यावर Summary message येईपर्यंत कृपया थांबा.",
		len(accesses), strings.Join(names, "\n"))

	if _, err := ctx.SendMessage(chatID, &tg.MessagesSendMessageRequest{
		Message: text, ReplyMarkup: adPromptMarkup(),
	}); err == nil {
		db.MarkADPrompted(user.ID, chatID)
	}
}

// handleAutoDeliveryChoice -- popup chya "Ho"/"Nako" buttons.
func handleAutoDeliveryChoice(ctx *ext.Context, u *ext.Update, chatID int64, yes bool) {
	user, err := db.GetUserByTelegramID(chatID)
	if err != nil || user == nil {
		editCatalogMessage(ctx, u, "❌ तुमचं Telegram account link केलेलं नाही.\n\n`/register <mobile_or_email>` वापरा.", nil)
		return
	}
	if !yes {
		db.SetADUserDeclined(user.ID, chatID)
		editCatalogMessage(ctx, u, "ठीक आहे 👍 आपोआप पाठवणार नाही.\n\nCourses मेनूमधून हवे ते lectures किंवा Folder/Course Download बटण वापरा.", nil)
		return
	}
	db.EnsureAutoDeliveryIndexes()
	db.SetADUserConfirmed(user.ID, chatID)
	editCatalogMessage(ctx, u,
		"✅ ठीक आहे! पाठवणं सुरू होत आहे.\n\n"+
			"⏳ कृपया थांबा — सगळे lectures folder-wise एकामागून एक येतील आणि शेवटी Summary message येईल.\n"+
			"तोपर्यंत bot चं काहीही बटण दाबू नका किंवा chat clear करू नका.", nil)
	_ = dispatcher.EndGroups
}
