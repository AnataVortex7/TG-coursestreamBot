// internal/commands/catalog.go
//
// Student-facing in-bot course browsing — जसं website वर दिसतं तसंच आता बॉट
// मध्येच: /courses → प्रत्येक course साठी 💳 Buy Now / ✅ Available बटण →
// Available असेल तर तो course उघडून folders दिसतात → folder उघडला की आतल्या
// lectures ची list buttons सकट दिसते → lecture बटण दाबलं की तीच लगेच खाली
// (watermark सकट) पाठवली जाते.
//
// Cache: प्रत्येक (user, course, lecture) साठी watermark लावलेली copy एकदाच
// तयार होते — ती LOG_CHANNEL मध्ये backup राहते (db.DeliveredCache). पुढच्या
// वेळी तोच user तीच lecture मागेल तेव्हा नवीन watermark job न चालवता थेट तिथून
// (जलद) forward होते.
package commands

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ═══════════════════════════════════════════════════════════════════════════
// Load — /courses command + wc: callback prefix
// ═══════════════════════════════════════════════════════════════════════════

// mainReplyKeyboardForUser — युझरच्या रोलनुसार (Admin / Userbot Access / Normal Student)
// चॅट बारमध्ये फक्त तेवढीच बटने दाखवतो जी त्याला वापराची परवानगी आहे.
func mainReplyKeyboardForUser(chatID int64) tg.ReplyMarkupClass {
	isAdmin := len(config.ValueOf.AllowedUsers) == 0 || utils.Contains(config.ValueOf.AllowedUsers, chatID)
	hasUserbot := db.HasUserbotAccess(chatID)

	if isAdmin {
		return &tg.ReplyKeyboardMarkup{
			Resize: true,
			Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "📚 Courses"},
					&tg.KeyboardButton{Text: "⭐ Userbot Settings"},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "💻 CMD"},
					&tg.KeyboardButton{Text: "⚡ Pre-download"},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "📥 Userbot Download"},
					&tg.KeyboardButton{Text: "📊 Bot Status"},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "❓ My ID"},
				}},
			},
		}
	}

	if hasUserbot {
		return &tg.ReplyKeyboardMarkup{
			Resize: true,
			Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "📚 Courses"},
					&tg.KeyboardButton{Text: "⭐ Userbot Settings"},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "📥 Userbot Download"},
					&tg.KeyboardButton{Text: "❓ My ID"},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButton{Text: "ℹ️ Help"},
				}},
			},
		}
	}

	// Normal Student (No Userbot Access):
	return &tg.ReplyKeyboardMarkup{
		Resize: true,
		Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButton{Text: "📚 Courses"},
				&tg.KeyboardButton{Text: "❓ My ID"},
			}},
			{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButton{Text: "ℹ️ Help"},
			}},
		},
	}
}

func mainReplyKeyboard() tg.ReplyMarkupClass {
	return mainReplyKeyboardForUser(0)
}

func coursesButtonMarkup() tg.ReplyMarkupClass {
	return mainReplyKeyboardForUser(0)
}

func (m *command) LoadCatalog(d dispatcher.Dispatcher) {
	log := m.log.Named("catalog")
	defer log.Sugar().Info("Loaded in-bot course catalog")

	d.AddHandler(handlers.NewCommand("courses", cmdCoursesRoot))
	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("wc:"), handleCatalogCallback))

	// Group -3 — admin च्या pending-menu catcher (menu.go, group -2) च्याही
	// आधी चालतो. Student (non-admin) कडूनही चालतो — gift-card flow admin-only
	// नाही. Pending नसेल तर लगेच ContinueGroups, बाकी काहीही disturb होत नाही.
	d.AddHandlerToGroup(handlers.NewMessage(nil, handleGiftCardReply), -3)
}

// ═══════════════════════════════════════════════════════════════════════════
// /courses — root list
// ═══════════════════════════════════════════════════════════════════════════

func cmdCoursesRoot(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	text, markup, err := renderCourseListView(chatID)
	if err != nil {
		reply(ctx, u, "❌ Courses लोड करताना error: "+err.Error())
		return dispatcher.EndGroups
	}
	ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
	return dispatcher.EndGroups
}

func renderCourseListView(chatID int64) (string, tg.ReplyMarkupClass, error) {
	courses, err := db.ListAllCourses()
	if err != nil {
		return "", nil, err
	}
	dbUser, _ := db.GetUserByTelegramID(chatID)

	var rows []tg.KeyboardButtonRow
	for _, c := range courses {
		if !c.IsActive {
			continue
		}
		hasAccess := false
		if dbUser != nil {
			hasAccess, _ = db.HasAccess(dbUser.ID, c.ID)
		}
		label := "💳 " + c.Title + " — Buy"
		if hasAccess {
			label = "✅ " + c.Title + " — Available"
		}
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: label, Data: []byte("wc:course:" + c.ID.Hex())},
		}})
	}
	if len(rows) == 0 {
		return "📚 सध्या कोणतेही courses उपलब्ध नाहीत.", &tg.ReplyInlineMarkup{}, nil
	}
	return "📚 *Courses*\n\nखालीलपैकी निवडा:", &tg.ReplyInlineMarkup{Rows: rows}, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Callback router
// ═══════════════════════════════════════════════════════════════════════════

func handleCatalogCallback(ctx *ext.Context, u *ext.Update) (retErr error) {
	// Telegram ला लगेच answer करतो — कारण जर पुढे काही error/panic आलं आणि
	// answer कधीच गेलं नाही, तर client वर button "stuck" (काहीच रिस्पॉन्स
	// नाही) असं दिसतं आणि पुढचे taps पण ignore होऊ शकतात. आधी answer, मग काम.
	if u.CallbackQuery != nil {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID})
	}

	// कुठलाही अनपेक्षित panic आला (nil pointer, इ.) तरी silently काहीच न
	// होता user ला निदान error तरी दिसावा, म्हणून recover.
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Sugar().Errorw("catalog: callback panic recovered", "panic", r)
			answerCatalogError(ctx, u, fmt.Errorf("अनपेक्षित error आली, पुन्हा प्रयत्न करा"))
			retErr = dispatcher.EndGroups
		}
	}()

	data := strings.TrimPrefix(string(u.CallbackQuery.Data), "wc:")
	parts := strings.Split(data, ":")
	if len(parts) == 0 {
		return dispatcher.EndGroups
	}
	chatID := u.EffectiveUser().GetID()

	switch parts[0] {
	case "noop":

	case "premium_menu":
		if !db.HasUserbotAccess(chatID) {
			editCatalogMessage(ctx, u, fmt.Sprintf("⛔ तुम्हाला Userbot Features चा ॲक्सेस नाही.\n\nAdmin कडून तुमच्या Telegram ID (`%d`) ला ॲक्सेस मिळवा.", chatID), &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
			return dispatcher.EndGroups
		}
		text, markup := renderPremiumMenuView(chatID)
		editCatalogMessage(ctx, u, text, markup)

	case "userbot_status":
		if !db.HasUserbotAccess(chatID) {
			editCatalogMessage(ctx, u, fmt.Sprintf("⛔ तुम्हाला Userbot Features चा ॲक्सेस नाही.\n\nAdmin कडून तुमच्या Telegram ID (`%d`) ला ॲक्सेस मिळवा.", chatID), &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
			return dispatcher.EndGroups
		}
		text, markup := renderUserbotStatusDetailView(chatID)
		editCatalogMessage(ctx, u, text, markup)

	case "userbot_dl":
		setPendingUserbotDLPrompt(ctx, u, chatID)

	case "userbot_login":
		cmdUserbotLoginStart(ctx, u)

	case "userbot_sendchannel":
		setPendingSendChannelPrompt(ctx, u, chatID)

	case "userbot_watermark":
		setPendingWatermarkPrompt(ctx, u, chatID)

	case "userbot_rm_watermark":
		watermarkStore.Delete(chatID)
		_ = db.SetUserWatermark(chatID, "")
		text, markup := renderPremiumMenuView(chatID)
		editCatalogMessage(ctx, u, "✅ Watermark clear केला!\n\n"+text, markup)

	case "userbot_logout":
		_ = db.DeleteUserBotSessionForUser(chatID)
		text, markup := renderPremiumMenuView(chatID)
		editCatalogMessage(ctx, u, "🚪 Userbot Account Logout केला!\n\n"+text, markup)

		// फक्त page-indicator बटण ("📄 2/5") — काहीच करायचं नाही, already answered वर.

	case "root":
		text, markup, err := renderCourseListView(chatID)
		if err != nil {
			answerCatalogError(ctx, u, err)
			return dispatcher.EndGroups
		}
		editCatalogMessage(ctx, u, text, markup)

	case "course":
		if len(parts) < 2 {
			return dispatcher.EndGroups
		}
		courseID, err := primitive.ObjectIDFromHex(parts[1])
		if err != nil {
			answerCatalogError(ctx, u, err)
			return dispatcher.EndGroups
		}
		text, markup, err := renderCourseDetailView(chatID, courseID)
		if err != nil {
			answerCatalogError(ctx, u, err)
			return dispatcher.EndGroups
		}
		editCatalogMessage(ctx, u, text, markup)

	case "folder":
		if len(parts) < 3 {
			return dispatcher.EndGroups
		}
		courseID, err1 := primitive.ObjectIDFromHex(parts[1])
		if err1 != nil {
			return dispatcher.EndGroups
		}
		folderID := primitive.NilObjectID
		if parts[2] != "ungrouped" {
			fid, err2 := primitive.ObjectIDFromHex(parts[2])
			if err2 != nil {
				return dispatcher.EndGroups
			}
			folderID = fid
		}
		page := 0
		if len(parts) >= 4 {
			_, _ = fmt.Sscanf(parts[3], "%d", &page)
		}
		text, markup, err := renderFolderContentsView(chatID, courseID, folderID, page)
		if err != nil {
			answerCatalogError(ctx, u, err)
			return dispatcher.EndGroups
		}
		editCatalogMessage(ctx, u, text, markup)

	case "regnew":
		if len(parts) < 2 {
			return dispatcher.EndGroups
		}
		courseID, err := primitive.ObjectIDFromHex(parts[1])
		if err != nil {
			return dispatcher.EndGroups
		}
		startNewUserRegistration(ctx, u, chatID, courseID)

	case "regold":
		if len(parts) < 2 {
			return dispatcher.EndGroups
		}
		courseID, err := primitive.ObjectIDFromHex(parts[1])
		if err != nil {
			return dispatcher.EndGroups
		}
		startOldUserLink(ctx, u, chatID, courseID)

	case "gift":
		if len(parts) < 2 {
			return dispatcher.EndGroups
		}
		courseID, err := primitive.ObjectIDFromHex(parts[1])
		if err != nil {
			return dispatcher.EndGroups
		}
		startGiftCardFlow(ctx, u, chatID, courseID)

	case "play":
		if len(parts) < 3 {
			return dispatcher.EndGroups
		}
		courseID, err1 := primitive.ObjectIDFromHex(parts[1])
		var msgID int
		if _, err := fmt.Sscanf(parts[2], "%d", &msgID); err != nil {
			return dispatcher.EndGroups
		}
		if err1 != nil {
			return dispatcher.EndGroups
		}
		_ = handlePlayLecture(ctx, u, chatID, courseID, msgID, false)

	case "dlfolder":
		if len(parts) < 3 {
			return dispatcher.EndGroups
		}
		courseID, err1 := primitive.ObjectIDFromHex(parts[1])
		if err1 != nil {
			return dispatcher.EndGroups
		}
		folderID := primitive.NilObjectID
		if parts[2] != "ungrouped" {
			fid, err2 := primitive.ObjectIDFromHex(parts[2])
			if err2 != nil {
				return dispatcher.EndGroups
			}
			folderID = fid
		}
		handlePlayFolder(ctx, u, chatID, courseID, folderID)

	case "dlcourse":
		if len(parts) < 2 {
			return dispatcher.EndGroups
		}
		courseID, err1 := primitive.ObjectIDFromHex(parts[1])
		if err1 != nil {
			return dispatcher.EndGroups
		}
		handlePlayCourse(ctx, u, chatID, courseID)
	}
	return dispatcher.EndGroups
}

// toUpdates — MessagesSendMedia/इतर API calls चा response (tg.UpdatesClass
// interface) सुरक्षितपणे *tg.Updates मध्ये convert करतो.
func toUpdates(v tg.UpdatesClass) *tg.Updates {
	if updates, ok := v.(*tg.Updates); ok {
		return updates
	}
	return nil
}

// extractNewMessageID — MessagesForwardMessages च्या response मधून नवीन
// तयार झालेल्या message चा ID काढतो (delete-tracking साठी).
func extractNewMessageID(updates *tg.Updates) int {
	if updates == nil {
		return 0
	}
	for _, upd := range updates.Updates {
		switch m := upd.(type) {
		case *tg.UpdateNewMessage:
			return m.Message.GetID()
		case *tg.UpdateNewChannelMessage:
			return m.Message.GetID()
		case *tg.UpdateMessageID:
			return m.ID
		}
	}
	return 0
}

// extractSentMessageID — [FIX] MessagesSendMessage च्या response मधून नवीन
// पाठवलेल्या message चा ID काढतो — private chat (user ला direct) आणि
// channel/group दोन्हीसाठी काम करतो.
//
// [BUG] आधी फक्त extractNewMessageID(toUpdates(resp)) वापरत होतो — पण
// toUpdates() फक्त *tg.Updates टाईप कडे cast करतो. Private chat (positive
// chatID, म्हणजे user ला थेट पाठवलेला message) मध्ये Telegram
// messages.sendMessage चं उत्तर *tg.Updates ऐवजी *tg.UpdateShortSentMessage
// असं वेगळंच (हलकं) टाईप देतो — त्यामुळे cast नेहमी fail व्हायचा, newMsgID
// नेहमी 0 यायचा, आणि handlePlayCourse मधला folder-name pin कधीच trigger
// व्हायचा नाही — तेही कुठलाही error न दाखवता (कारण "ID सापडला नाही" ही आपली
// स्वतःची अंतर्गत skip-condition होती, error case नाही).
func extractSentMessageID(v tg.UpdatesClass) int {
	switch m := v.(type) {
	case *tg.UpdateShortSentMessage:
		return m.ID
	case *tg.Updates:
		return extractNewMessageID(m)
	default:
		return 0
	}
}

func answerCatalogError(ctx *ext.Context, u *ext.Update, err error) {
	if isMessageNotModifiedErr(err) {
		return
	}
	editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Text: "⬅️ Courses", Data: []byte("wc:root")}}},
	}})
}

// editCatalogMessage — catalog navigation साठी in-place message edit करतो.
//
// [FIX] आधी FLOOD_WAIT error सुद्धा "कुठलाही edit-fail" म्हणून treat व्हायचा —
// लगेच fallback म्हणून नवीन message पाठवायचा. bulk delivery (handlePlayCourse
// — "संपूर्ण course download") मध्ये हेच progress-message प्रत्येक lecture
// नंतर edit होतं (सेकंदाला एकदा) — त्यामुळे पहिल्याच FLOOD_WAIT नंतर, actual
// wait न करता, दर काही सेकंदांनी परत तोच edit + fallback-send दोन्ही होत
// राहायचे. Telegram कडून प्रत्येक वेळी नवीन (पण कमी होत जाणारा) FLOOD_WAIT
// यायचा — कारण खरा wait अजून संपलेलाच नव्हता, आणि दरम्यान आणखी नवीन spam
// messages (fallback) पाठवले जात राहिल्याने flood आणखी वाढायचा.
//
// आता: FLOOD_WAIT आढळला तर सेकंद parse करून तेवढा (जास्तीत जास्त
// floodEditWaitCap इतका, म्हणजे bulk loop मध्ये एका lecture साठी अनंत काळ
// block होत नाही) प्रत्यक्ष थांबतो, मग एकदाच retry करतो. अजूनही FLOOD_WAIT
// असेल तर fallback-spam न करता फक्त skip करतो (warn log सह) — पुढच्या
// lectures कडे continue होतं, आणि flood आणखी वाढत नाही. FLOOD_WAIT नसलेला
// इतर कुठलाही error असेल तरच जुनीच पद्धत (log + एकच fallback message).
const floodEditWaitCap = 15 * time.Second


var (
	suppressEditMu    sync.Mutex
	suppressEditUntil = make(map[int]time.Time)
)

func suppressMessageEdit(msgID int, waitSecs int) {
	if msgID == 0 {
		return
	}
	suppressEditMu.Lock()
	defer suppressEditMu.Unlock()
	suppressEditUntil[msgID] = time.Now().Add(time.Duration(waitSecs) * time.Second)
}

func isMessageEditSuppressed(msgID int) bool {
	if msgID == 0 {
		return false
	}
	suppressEditMu.Lock()
	defer suppressEditMu.Unlock()
	until, ok := suppressEditUntil[msgID]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(suppressEditUntil, msgID)
	return false
}

func editCatalogMessage(ctx *ext.Context, u *ext.Update, text string, markup tg.ReplyMarkupClass) {
	cbq := u.CallbackQuery
	if cbq == nil {
		_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
		return
	}
	chatID := u.EffectiveUser().GetID()
	if isMessageEditSuppressed(cbq.MsgID) {
		return
	}

	_, err := ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
		ID:          cbq.MsgID,
		Message:     text,
		ReplyMarkup: markup,
	})
	if err == nil {
		return
	}
	if isMessageNotModifiedErr(err) {
		return
	}
	if waitSecs, isFlood := extractFloodWaitSeconds(err); isFlood {
		suppressMessageEdit(cbq.MsgID, waitSecs)
		utils.Logger.Sugar().Warnw("catalog: EditMessage FLOOD_WAIT detected — suppressing edits for this message",
			"chat_id", chatID, "msg_id", cbq.MsgID, "wait_secs", waitSecs)
		return
	}

	utils.Logger.Sugar().Errorw("catalog: EditMessage failed, sending fallback message",
		"chat_id", chatID, "msg_id", cbq.MsgID, "err", err)
	if _, sendErr := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup}); sendErr != nil {
		utils.Logger.Sugar().Errorw("catalog: fallback message send also failed", "chat_id", chatID, "err", sendErr)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Course detail — Buy (no access) / Folders (access)
// ═══════════════════════════════════════════════════════════════════════════

func renderCourseDetailView(chatID int64, courseID primitive.ObjectID) (string, tg.ReplyMarkupClass, error) {
	course, err := db.GetCourseByID(courseID)
	if err != nil {
		return "", nil, err
	}
	if course == nil {
		return "❌ Course सापडला नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")}, nil
	}

	dbUser, _ := db.GetUserByTelegramID(chatID)
	hasAccess := false
	if dbUser != nil {
		hasAccess, _ = db.HasAccess(dbUser.ID, course.ID)
	}

	if !hasAccess {
		text := fmt.Sprintf("📚 *%s*\n\n%s\n\n💰 किंमत: %s", course.Title, course.Description, course.Price)
		var rows []tg.KeyboardButtonRow

		mode := course.PaymentMode
		if mode == "" {
			// Auto: BuyLink असेल तर तोच वापरतो, नसेल तर gift-card fallback —
			// जेणेकरून admin ने काहीच सेट न केलं तरी Buy बटण कधीच तुटलेलं दिसणार नाही.
			if course.BuyLink != "" {
				mode = "link"
			} else {
				mode = "giftcard"
			}
		}
		if mode == "link" && course.BuyLink != "" {
			rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonURL{Text: "💳 Buy Now", URL: course.BuyLink},
			}})
		} else {
			rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: "🎁 Gift Card ने Pay करा", Data: []byte("wc:gift:" + course.ID.Hex())},
			}})
		}
		rows = append(rows, backOnlyRow("wc:root")...)
		return text, &tg.ReplyInlineMarkup{Rows: rows}, nil
	}

	// ── Access आहे — folders दाखवतो ──────────────────────────────────────
	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		return "", nil, err
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		return "", nil, err
	}
	hasUngrouped := false
	for _, f := range files {
		if db.FolderForSortOrder(folders, f.SortOrder) == nil {
			hasUngrouped = true
			break
		}
	}

	var rows []tg.KeyboardButtonRow
	for _, fo := range folders {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📁 " + fo.Name, Data: []byte("wc:folder:" + courseID.Hex() + ":" + fo.ID.Hex() + ":0")},
		}})
	}
	if hasUngrouped {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📁 Ungrouped", Data: []byte("wc:folder:" + courseID.Hex() + ":ungrouped:0")},
		}})
	}
	if len(rows) == 0 {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "ℹ️ अजून content upload झालेला नाही", Data: []byte("wc:root")},
		}})
	} else {
		// [NEW] संपूर्ण course एका टॅपमध्ये डाउनलोड — folders असो वा नसो,
		// आतले सगळे files (video/PDF/image) मिळतात. handlePlayCourse हेच
		// फोल्डर-नावासकट पाठवतो.
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: fmt.Sprintf("📥 पूर्ण Course Download करा (%d)", len(files)), Data: []byte("wc:dlcourse:" + courseID.Hex())},
		}})
	}
	rows = append(rows, backOnlyRow("wc:root")...)
	return fmt.Sprintf("✅ *%s*\n\n📂 Folder निवडा:", course.Title), &tg.ReplyInlineMarkup{Rows: rows}, nil
}

func backOnlyRow(data string) []tg.KeyboardButtonRow {
	return []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "⬅️ Back", Data: []byte(data)},
	}}}
}

// ═══════════════════════════════════════════════════════════════════════════
// Folder contents — lecture list with ▶️ buttons
// ═══════════════════════════════════════════════════════════════════════════

// lecturesPerPage — एका screen वर जास्तीत जास्त किती lecture buttons.
// मोठ्या folder मध्ये (khup lectures) सगळे buttons एकाच वेळी पाठवायचा
// प्रयत्न केला की reply-markup खूप मोठं होतं आणि Telegram edit/send
// तिथेच silently fail व्हायचं (folder "उघडायचंच नाही" असं दिसायचं) —
// pagination नी हे कायमचं टाळलं जातं, folder मध्ये कितीही lectures असोत.
const lecturesPerPage = 5

func renderFolderContentsView(chatID int64, courseID, folderID primitive.ObjectID, page int) (string, tg.ReplyMarkupClass, error) {
	dbUser, _ := db.GetUserByTelegramID(chatID)
	hasAccess := false
	if dbUser != nil {
		hasAccess, _ = db.HasAccess(dbUser.ID, courseID)
	}
	if !hasAccess {
		return "⛔ या course चा access नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())}, nil
	}

	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		return "", nil, err
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		return "", nil, err
	}

	// या user ने course मध्ये आत्तापर्यंत कुठले lectures मागवले आहेत —
	// button वर "✅" mark दाखवायला (एकाच DB call मध्ये, प्रत्येक lecture साठी वेगळा call टाळून).
	var watched map[int]bool
	if dbUser != nil {
		watched, _ = db.GetDeliveredMessageIDs(dbUser.ID, courseID)
	}

	folderName := "Ungrouped"
	isUngrouped := folderID == primitive.NilObjectID

	// आधी matching files ची पूर्ण list बनवतो (unpaginated), मग त्यातला
	// फक्त सध्याच्या page चा तुकडा buttons म्हणून दाखवतो.
	type item struct {
		title     string
		messageID int
	}
	var matched []item
	for _, f := range files {
		fo := db.FolderForSortOrder(folders, f.SortOrder)
		if isUngrouped {
			if fo != nil {
				continue
			}
		} else {
			if fo == nil || fo.ID != folderID {
				continue
			}
			folderName = fo.Name
		}
		// Button वर actual real file name (उदा. xyz.mp4, xyz.pdf) दाखवतो —
		// Title पेक्षा हेच जास्त उपयोगी असतं (user ला नेमकं ओळखता येतं).
		title := f.FileName
		if title == "" {
			title = f.Title
		}
		matched = append(matched, item{title: title, messageID: f.MessageID})
	}

	folderRef := "ungrouped"
	if !isUngrouped {
		folderRef = folderID.Hex()
	}

	totalPages := (len(matched) + lecturesPerPage - 1) / lecturesPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}
	start := page * lecturesPerPage
	end := start + lecturesPerPage
	if end > len(matched) {
		end = len(matched)
	}

	var rows []tg.KeyboardButtonRow
	if start < end {
		for _, it := range matched[start:end] {
			// आधी पाहिलेलं/डाउनलोड केलेलं lecture असेल तर वेगळं mark (✅),
			// नसेल तर नेहमीचं ▶️ — user ला एका नजरेत कळेल काय बाकी आहे.
			icon := "▶️"
			if watched != nil && watched[it.messageID] {
				icon = "✅"
			}
			rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: icon + " " + it.title, Data: []byte(fmt.Sprintf("wc:play:%s:%d", courseID.Hex(), it.messageID))},
			}})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "ℹ️ या folder मध्ये काही नाही", Data: []byte("wc:course:" + courseID.Hex())},
		}})
	} else {
		// [NEW] संपूर्ण folder एका टॅपमध्ये डाउनलोड करायचं बटण — प्रत्येक lecture
		// साठी हाच fast-path/cache/watermark logic वापरतो (handlePlayLecture),
		// त्यामुळे आधीच पाठवलेले (✅) परत log-channel वरून लगेच येतात, नवीन
		// तेच नव्याने watermark होऊन जातात.
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: fmt.Sprintf("📥 संपूर्ण Folder Download करा (%d)", len(matched)), Data: []byte(fmt.Sprintf("wc:dlfolder:%s:%s", courseID.Hex(), folderRef))},
		}})
	}

	// Prev/Next + मोठ्या folder साठी (50+ pages) windowed jump navigation —
	// सगळे page numbers दाखवणं (grid) 50 pages वर अव्यवहार्य ठरतं, त्यामुळे:
	//   Row 1: ⏮ First | ⬅️ Prev | Next ➡️ | Last ⏭
	//   Row 2: ⏪-10 | (सध्याच्या page भोवतीचे ~6 numbers) | +10⏩
	// एकापेक्षा जास्त page असेल तरच दाखवतो.
	if totalPages > 1 {
		var navRow1 []tg.KeyboardButtonClass
		if page > 0 {
			navRow1 = append(navRow1,
				&tg.KeyboardButtonCallback{Text: "⏮ First", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, 0))},
				&tg.KeyboardButtonCallback{Text: "⬅️ Prev", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, page-1))},
			)
		}
		navRow1 = append(navRow1, &tg.KeyboardButtonCallback{
			Text: fmt.Sprintf("📄 %d/%d", page+1, totalPages), Data: []byte("wc:noop"),
		})
		if page < totalPages-1 {
			navRow1 = append(navRow1,
				&tg.KeyboardButtonCallback{Text: "Next ➡️", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, page+1))},
				&tg.KeyboardButtonCallback{Text: "Last ⏭", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, totalPages-1))},
			)
		}
		rows = append(rows, tg.KeyboardButtonRow{Buttons: navRow1})

		// [NEW] सध्याच्या page भोवतीचा window (जास्तीत जास्त 6 numbers) — मोठ्या
		// jump साठी ±10 buttons दोन्ही टोकांना (totalPages लहान असेल तर आपोआप
		// window पूर्ण range कव्हर करतो, वेगळे ±10 buttons लागत नाहीत).
		const windowSize = 6
		winStart := page - windowSize/2
		if winStart < 0 {
			winStart = 0
		}
		winEnd := winStart + windowSize - 1
		if winEnd > totalPages-1 {
			winEnd = totalPages - 1
			winStart = winEnd - windowSize + 1
			if winStart < 0 {
				winStart = 0
			}
		}

		var navRow2 []tg.KeyboardButtonClass
		if winStart > 0 {
			jumpBack := winStart - 10
			if jumpBack < 0 {
				jumpBack = 0
			}
			navRow2 = append(navRow2, &tg.KeyboardButtonCallback{
				Text: "⏪-10", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, jumpBack)),
			})
		}
		for p := winStart; p <= winEnd; p++ {
			label := fmt.Sprintf("%d", p+1)
			if p == page {
				label = "·" + label + "·" // सध्याचं page highlight
			}
			navRow2 = append(navRow2, &tg.KeyboardButtonCallback{
				Text: label, Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, p)),
			})
		}
		if winEnd < totalPages-1 {
			jumpFwd := winEnd + 10
			if jumpFwd > totalPages-1 {
				jumpFwd = totalPages - 1
			}
			navRow2 = append(navRow2, &tg.KeyboardButtonCallback{
				Text: "+10⏩", Data: []byte(fmt.Sprintf("wc:folder:%s:%s:%d", courseID.Hex(), folderRef, jumpFwd)),
			})
		}
		if len(navRow2) > 0 {
			rows = append(rows, tg.KeyboardButtonRow{Buttons: navRow2})
		}
	}

	rows = append(rows, backOnlyRow("wc:course:"+courseID.Hex())...)
	return fmt.Sprintf("📂 *%s*\n\nलेक्चर निवडा:\n✅ = आधीच पाहिलं/डाउनलोड केलं", folderName), &tg.ReplyInlineMarkup{Rows: rows}, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Play — deliver the lecture in-place (watermark + per-user cache)
// ═══════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════
// Admin — Payment Mode (Buy Link / Gift Card) per course
// ═══════════════════════════════════════════════════════════════════════════

// paymentModeItems — admin menu.go च्या selectStep साठी static 2-item list.
func paymentModeItems() ([]listItem, string, error) {
	return []listItem{
		{display: "💳 Buy Link (course चा BuyLink वापरून, बाहेर payment)", id: "link"},
		{display: "🎁 Gift Card (बॉट मध्येच — user गिफ्ट कार्ड नंबर/PIN पाठवतो, तुम्हाला notify होतं)", id: "giftcard"},
	}, "", nil
}

// cmdSetPaymentMode — /setpaymentmode <courseIDHex> <link|giftcard>
// (menu.go flow: आधी course निवडा, मग payment mode निवडा — दोन्ही collected args
// इथे a[0]=courseID, a[1]=mode म्हणून येतात.)
func cmdSetPaymentMode(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "❌ वापर: course आणि mode दोन्ही निवडा.")
		return dispatcher.EndGroups
	}
	courseID, err := primitive.ObjectIDFromHex(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}
	mode := a[1]
	if mode != "link" && mode != "giftcard" {
		reply(ctx, u, "❌ Invalid mode.")
		return dispatcher.EndGroups
	}
	if err := db.UpdateCourse(courseID, bson.M{"payment_mode": mode}); err != nil {
		reply(ctx, u, "❌ Error: "+err.Error())
		return dispatcher.EndGroups
	}
	label := "💳 Buy Link"
	if mode == "giftcard" {
		label = "🎁 Gift Card"
	}
	reply(ctx, u, "✅ Payment mode set: "+label)
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Gift Card purchase flow (in-bot) — BuyLink नसेल/giftcard mode असेल तेव्हा.
// Website च्या /api/purchase सारखंच: card number → PIN → message (optional) →
// submit झाल्यावर admin ला Telegram + email दोन्हीकडे notify होतं.
// ═══════════════════════════════════════════════════════════════════════════

type giftCardPhase string

const (
	giftPhaseCard    giftCardPhase = "card"
	giftPhasePIN     giftCardPhase = "pin"
	giftPhaseMessage giftCardPhase = "message"
)

type giftCardState struct {
	CourseID   primitive.ObjectID
	Phase      giftCardPhase
	CardNumber string
	CardPIN    string
	ExpiresAt  time.Time
}

const giftCardTTL = 5 * time.Minute

var (
	giftMu      sync.Mutex
	giftPending = map[int64]*giftCardState{}
)

func setGiftPending(chatID int64, st *giftCardState) {
	st.ExpiresAt = time.Now().Add(giftCardTTL)
	giftMu.Lock()
	defer giftMu.Unlock()
	giftPending[chatID] = st
}

func getGiftPending(chatID int64) *giftCardState {
	giftMu.Lock()
	defer giftMu.Unlock()
	st, ok := giftPending[chatID]
	if !ok {
		return nil
	}
	if time.Now().After(st.ExpiresAt) {
		delete(giftPending, chatID)
		return nil
	}
	return st
}

func clearGiftPending(chatID int64) {
	giftMu.Lock()
	defer giftMu.Unlock()
	delete(giftPending, chatID)
}

func startGiftCardFlow(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID) {
	dbUser, _ := db.GetUserByTelegramID(chatID)
	if dbUser == nil {
		editCatalogMessage(ctx, u,
			"👋 Gift Card payment करण्याआधी account link करूया.\n\nतुम्ही:",
			&tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "🆕 नवीन User (New)", Data: []byte("wc:regnew:" + courseID.Hex())},
				}},
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "👤 आधीच Account आहे (Old)", Data: []byte("wc:regold:" + courseID.Hex())},
				}},
				{Buttons: backOnlyRow("wc:course:" + courseID.Hex())[0].Buttons},
			}})
		return
	}
	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		editCatalogMessage(ctx, u, "❌ Course सापडला नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		return
	}
	setGiftPending(chatID, &giftCardState{CourseID: courseID, Phase: giftPhaseCard})
	editCatalogMessage(ctx, u,
		fmt.Sprintf("🎁 *Gift Card Payment — %s*\n\n1️⃣ Gift Card Number टाका:", course.Title),
		&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
}

// handleGiftCardReply — plain-text catcher, admin च्या handlePendingReply (menu.go)
// पेक्षा वेगळा — student (non-admin) कडूनही चालतो. Group -3 वर, म्हणजे बाकी
// सगळ्या handlers च्याही आधी check होतो; pending नसेल तर लगेच ContinueGroups.
func handleGiftCardReply(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	if msg == nil || strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return dispatcher.ContinueGroups
	}
	chatID := u.EffectiveUser().GetID()
	st := getGiftPending(chatID)
	if st == nil {
		return dispatcher.ContinueGroups
	}
	text := strings.TrimSpace(msg.Text)
	backRow := backOnlyRow("wc:course:" + st.CourseID.Hex())

	switch st.Phase {
	case giftPhaseCard:
		st.CardNumber = text
		st.Phase = giftPhasePIN
		setGiftPending(chatID, st)
		reply(ctx, u, "2️⃣ Gift Card PIN टाका:")
		return dispatcher.EndGroups

	case giftPhasePIN:
		st.CardPIN = text
		st.Phase = giftPhaseMessage
		setGiftPending(chatID, st)
		reply(ctx, u, "3️⃣ काही message असेल तर टाका (नसेल तर - पाठवा):")
		return dispatcher.EndGroups

	case giftPhaseMessage:
		message := text
		if message == "-" {
			message = ""
		}
		clearGiftPending(chatID)
		submitGiftCardPurchase(ctx, u, chatID, st.CourseID, st.CardNumber, st.CardPIN, message, backRow)
		return dispatcher.EndGroups
	}
	clearGiftPending(chatID)
	return dispatcher.ContinueGroups
}

// submitGiftCardPurchase — website च्या handlePurchase सारखंच: admin ला
// Telegram (सर्व AllowedUsers) + email दोन्हीकडे notify करतो.
func submitGiftCardPurchase(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID, cardNumber, cardPIN, message string, backRow []tg.KeyboardButtonRow) {
	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		reply(ctx, u, "❌ Account link सापडला नाही. /register पुन्हा करा.")
		return
	}
	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		reply(ctx, u, "❌ Course सापडला नाही.")
		return
	}

	notifyMsg := fmt.Sprintf(
		"🛒 *New Gift Card Purchase Submitted (via Bot)*\n\n"+
			"👤 User: %s\n📱 Mobile: %s\n📧 Email: %s\n"+
			"🎓 Course: %s (%s)\n"+
			"💳 Gift Card: `%s`\n🔑 PIN: `%s`\n📝 Message: %s",
		dbUser.FullName, dbUser.Mobile, dbUser.Email,
		course.Title, course.Price, cardNumber, cardPIN, orDash(message),
	)
	botClient := MainBotClient
	if botClient != nil {
		for _, adminID := range []int64(config.ValueOf.AllowedUsers) {
			adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
			if adminPeer == nil {
				continue
			}
			_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     adminPeer,
				Message:  notifyMsg,
				RandomID: rand.Int63(),
			})
		}
	}

	// Website सारखाच email notify (best-effort — fail झालं तरी user ला submit confirm दाखवतो,
	// कारण Telegram notify वरचं आधीच झालेलं आहे).
	go func() {
		adminEmail := os.Getenv("ADMIN_EMAIL")
		info := email.PurchaseInfo{
			UserFullName: dbUser.FullName,
			UserMobile:   dbUser.Mobile,
			UserEmail:    dbUser.Email,
			Username:     dbUser.Username,
			CourseTitle:  course.Title,
			CoursePrice:  course.Price,
			CourseID:     course.ID.Hex(),
			CardNumber:   cardNumber,
			CardPIN:      cardPIN,
			Message:      message,
		}
		if err := email.SendPurchaseNotification(adminEmail, info); err != nil {
			utils.Logger.Sugar().Warnw("catalog: gift-card email notify failed", "err", err)
		}
	}()

	reply(ctx, u, "✅ Submit झालं! Admin verify करून लवकरच access देईल — तोपर्यंत धन्यवाद 🙏")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

var (
	playMu   sync.Mutex
	playBusy = map[string]bool{} // "chatID:msgID" — double-tap guard
)

func lockPlay(key string) bool {
	playMu.Lock()
	defer playMu.Unlock()
	if playBusy[key] {
		return false
	}
	playBusy[key] = true
	return true
}

func unlockPlay(key string) {
	playMu.Lock()
	defer playMu.Unlock()
	delete(playBusy, key)
}

// alertAdminsUnwatermarkedDelivery — Colab watermark worker offline असल्यामुळे
// एखादी lecture protection शिवाय (plain) student ला गेली की admins ला कळवतो.
// Piracy-protection cha मुख्य मुद्दाच watermark आहे, त्यामुळे हे silently
// होऊ नये — admin ने `/colabstatus` check करून worker परत सुरू करावा.
func alertAdminsUnwatermarkedDelivery(ctx *ext.Context, dbUser *db.User, course *db.Course, studentChatID int64) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}
	msg := fmt.Sprintf(
		"⚠️ *Watermark worker OFFLINE* — खालील lecture *unwatermarked* पाठवली गेली:\n\n"+
			"🎓 Course: %s\n👤 Student: %s (%s)\n\n"+
			"👉 `/colabstatus` check करा आणि worker परत सुरू करा.",
		course.Title, dbUser.FullName, dbUser.Mobile,
	)
	for _, adminID := range []int64(config.ValueOf.AllowedUsers) {
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

func handlePlayLecture(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID, messageID int, isBulk bool) error {
	busyKey := fmt.Sprintf("%d:%d", chatID, messageID)
	if !lockPlay(busyKey) {
		if u.CallbackQuery != nil && !isBulk {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "⏳ आधीच पाठवत आहोत, थोडं थांबा...",
			})
		}
		return fmt.Errorf("आधीच पाठवत आहोत")
	}
	defer unlockPlay(busyKey)

	if u.CallbackQuery != nil && !isBulk {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID})
	}

	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		if !isBulk {
			editCatalogMessage(ctx, u, "❌ तुमचा Telegram account link केलेला नाही.\n\n`/register <mobile_or_email>` वापरा.",
				&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		}
		return fmt.Errorf("account not linked")
	}
	if blocked, until, reason := db.IsUserBlocked(dbUser.ID); blocked {
		if !isBulk {
			editCatalogMessage(ctx, u, fmt.Sprintf("⛔ Account suspended.\n📅 Until: %s IST\n📝 Reason: %s",
				until.In(istLoc()).Format("02 Jan 2006, 3:04 PM"), reason),
				&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		}
		return fmt.Errorf("account suspended")
	}
	hasAccess, _ := db.HasAccess(dbUser.ID, courseID)
	if !hasAccess {
		if !isBulk {
			editCatalogMessage(ctx, u, "⛔ या course चा access नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		}
		return fmt.Errorf("no access")
	}

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		if !isBulk {
			editCatalogMessage(ctx, u, "❌ Course सापडला नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		}
		return fmt.Errorf("course not found")
	}

	backRow := backOnlyRow("wc:course:" + courseID.Hex())
	if !isBulk {
		editCatalogMessage(ctx, u, "⏳ Lecture तयार करत आहोत...", &tg.ReplyInlineMarkup{Rows: backRow})
	}

	botClient := MainBotClient
	if botClient == nil {
		if !isBulk {
			editCatalogMessage(ctx, u, "❌ Bot सध्या तयार नाही. थोड्या वेळाने प्रयत्न करा.", &tg.ReplyInlineMarkup{Rows: backRow})
		}
		return fmt.Errorf("bot not ready")
	}

	progressMsgID := 0
	if u.CallbackQuery != nil && !isBulk {
		progressMsgID = u.CallbackQuery.MsgID
	}
	editProgress := func(text string) { 
		if !isBulk {
			go editCatalogMessage(ctx, u, text, &tg.ReplyInlineMarkup{Rows: backRow}) 
		}
	}

	delivered, dErr := deliverLectureCore(ctx, botClient, dbUser, course, courseID, messageID, chatID, progressMsgID, false, editProgress)
	if dErr != nil {
		if !isBulk {
			editCatalogMessage(ctx, u, "❌ "+dErr.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		}
		return dErr
	}
	if delivered && !isBulk {
		editCatalogMessage(ctx, u, "✅ लेक्चर खाली पाठवलं आहे 👇", &tg.ReplyInlineMarkup{Rows: backRow})
	}
	return nil
}

// deliverLectureCore — [NEW, refactored out of handlePlayLecture] एका lecture
// (messageID) ची delivery करणारा मुख्य engine — cache-hit fast-path, video
// watermark (targetUser च्या mobile/email सह), PDF/इतर फाईल्स, LOG_CHANNEL
// backup, DeliveredCache save — सगळं इथे आहे. `u *ext.Update`/callback वर
// अवलंबून नाही (फक्त `progressMsgID` + `editProgress` callback घेतो), त्यामुळे
// callback-driven (handlePlayLecture) आणि command-driven (admin bulk
// pre-download) दोन्हीकडून सारखंच वापरता येतं.
//
// targetChatID — video प्रत्यक्ष कोणाला पाठवायचं (विनंती करणारा user, किंवा
// admin pre-download असेल तर target user).
// dbUser — watermark text (mobile/email) आणि DeliveredCache key यासाठी —
// नेहमी targetChatID शीच जोडलेला असायला हवा (admin bulk मध्ये target user
// चाच dbUser पास करायचा, admin चा नाही).
// progressMsgID — watermark worker च्या live progress-edit साठी (0 चालतं,
// तेव्हा फक्त skip होतं).
// silent — [NEW] true असेल तर (admin चा silent pre-download) video/file फक्त
// LOG_CHANNEL मध्ये (cache साठी) जातं — targetChatID ला काहीच पाठवलं जात
// नाही. आधीच cache असेल तर काहीच करावं लागत नाही (already-cached मानतो).
//
// Returns delivered=true आणि err=nil म्हणजे यशस्वी (नवीन असो किंवा cache-hit).
// deterministicRandomID — Telegram च्या MessagesSendMedia/SendMessage चा
// RandomID field हा duplicate-detection साठी असतो: same RandomID सोबत same
// request परत पाठवली (उदा. network timeout नंतर retry, किंवा पूर्ण delivery
// परत चालवली) तर Telegram नवीन message न बनवता आधीचीच (असल्यास) परत देतो.
//
// [FIX] आधी प्रत्येक send attempt साठी rand.Int63() (कायम नवीन, random) वापरत
// होतो — त्यामुळे हे duplicate-protection कधीच काम करत नव्हतं: जर पहिला
// LOG_CHANNEL upload सर्व्हरवर प्रत्यक्ष यशस्वी झाला पण response client कडे
// पोहोचेपर्यंत network glitch/timeout आला (client ला error दिसला), तरी
// DeliveredCache save होत नाही (कारण आपल्याला यशस्वी झाल्याचं कळलंच नाही) —
// आणि पुढच्या वेळी (retry, किंवा /predownload परत चालवलं) तेच lecture परत नवीन
// (वेगळा) RandomID घेऊन पाठवलं जायचं — परिणामी तेच PDF/photo LOG_CHANNEL मध्ये
// डुप्लिकेट होत राहायचं, तरी वरचा counter मात्र "failed" च दाखवायचा.
//
// आता प्रत्येक (user, course, lecture) साठी हाच RandomID कायम सारखाच
// (deterministic — hash वरून) असतो, त्यामुळे कितीही वेळा retry/re-run झालं तरी
// Telegram स्वतःच duplicate ला रोखतो — एकच खरी copy तयार होते.
func deterministicRandomID(parts ...string) int64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	v := h.Sum64()
	v &= 0x7FFFFFFFFFFFFFFF // sign bit clear — Telegram ला positive/non-zero int64 लागतो
	if v == 0 {
		v = 1
	}
	return int64(v)
}

func deliverLectureCore(
	ctx *ext.Context,
	botClient *gotgproto.Client,
	dbUser *db.User,
	course *db.Course,
	courseID primitive.ObjectID,
	messageID int,
	targetChatID int64,
	progressMsgID int,
	silent bool,
	editProgress func(string),
) (delivered bool, err error) {
	// ── Cache hit — आधीच watermark लावलेली copy LOG_CHANNEL मध्ये आहे ─────
	if cached, cErr := db.GetDeliveredCache(dbUser.ID, courseID, messageID); cErr == nil && cached != nil && cached.LogChannelMsgID != 0 {
		if silent {
			// आधीच cache मध्ये आहे — silent pre-download साठी इतकंच पुरेसं,
			// targetChatID ला काहीही पाठवायची गरज नाही.
			utils.Logger.Sugar().Infow("catalog: silent pre-download — already cached, skipping", "messageID", messageID)
			return true, nil
		}
		utils.Logger.Sugar().Infow("catalog: DeliveredCache hit — trying fast-path forward",
			"logChannelMsgID", cached.LogChannelMsgID, "chatID", targetChatID)
		if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, cached.LogChannelMsgID, targetChatID); fErr == nil {
			if newMsgID := extractNewMessageID(fwdUpdates); newMsgID != 0 {
				_ = db.RecordSentLectureMsg(dbUser.ID, courseID, targetChatID, newMsgID)
			}
			return true, nil
		} else {
			if waitSecs, isFlood := extractFloodWaitSeconds(fErr); isFlood {
				utils.Logger.Sugar().Warnw("catalog: fast-path forward failed with FLOOD_WAIT — aborting delivery", "waitSecs", waitSecs)
				return false, fErr
			}
			utils.Logger.Sugar().Warnw("catalog: fast-path forward failed — falling back to fresh delivery", "err", fErr)
		}
		// Cache forward fail झालं (log channel मधून message delete झाला असेल) —
		// खालच्या fresh-delivery मार्गावर पडतो.
	} else if cErr != nil {
		utils.Logger.Sugar().Warnw("catalog: GetDeliveredCache query error", "err", cErr)
	} else {
		utils.Logger.Sugar().Infow("catalog: DeliveredCache miss — no cached entry for this user/lecture",
			"userID", dbUser.ID.Hex(), "courseID", courseID.Hex(), "messageID", messageID)
	}

	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, course.ChannelID)
	if err != nil {
		return false, fmt.Errorf("Channel access error: %s", err.Error())
	}
	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channelPeer,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
	})
	if err != nil {
		return false, fmt.Errorf("Message fetch error: %s", err.Error())
	}
	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		return false, fmt.Errorf("Lecture सापडली नाही.")
	}
	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		return false, fmt.Errorf("Media सापडला नाही.")
	}

	toPeer := ctx.PeerStorage.GetInputPeerById(targetChatID)

	var inputMedia tg.InputMediaClass
	isVideo := false
	watermarked := false
	wmEnabled := db.GetSettingBool(db.SettingOfflineWatermarkEnabled, true)

	// Lecture cha exact naav — तेच जे folder list मध्ये button वर दिसलं होतं
	// (actual file name — CachedFile.FileName), जेणेकरून user ला लगेच कळेल हा तोच video/file आहे.
	lectureName := course.Title
	// [FIX] cf.Caption असेल (म्हणजे admin ने त्या file साठी खरा caption सेव्ह
	// केलाय) तर तोच — word-replace rules (applyReplaces) लावून — प्रत्यक्ष
	// file सोबत पाठवला जातो. आधी हे कायम "🎬 "+lectureName असायचं, त्यामुळे
	// caption मध्ये admin ने केलेला कुठलाही word-change कधीच student ला दिसत
	// नव्हता. Listing/button वर मात्र फाइलचं नावच (lectureName) दाखवत राहतो —
	// तो वेगळा behavior आहे, इथे बदलेला नाही.
	var caption string
	if cf, cfErr := db.GetCachedFile(messageID, course.ChannelID); cfErr == nil && cf != nil {
		if cf.FileName != "" {
			lectureName = cf.FileName
		} else if cf.Title != "" {
			lectureName = cf.Title
		}
		if cf.Caption != "" {
			caption = applyReplaces(cf.Caption)
		}
	} else if msg.Message != "" {
		lectureName = msg.Message
	}
	if caption == "" {
		caption = "🎬 " + lectureName
	}
	sentMessageID := 0
	logChannelMsgID := 0 // viaColab path मध्ये worker कडून मिळालेला LOG_CHANNEL master msg ID (असल्यास)
	// [FIX] Python worker "sent_message_id" हा field कधीच पाठवत नाही (फक्त
	// "log_channel_msg_id" पाठवतो) — त्यामुळे viaColab delivery साठी
	// sentMessageID कायम 0 च राहतो, जरी delivery प्रत्यक्ष यशस्वी झाली तरी.
	// फक्त "sentMessageID != 0" वर अवलंबून राहिलो तर DeliveredCache कधीच
	// save होत नाही (✅ tick कधीच येत नाही, पुढच्या वेळी परत fresh job चालतो).
	// हा flag त्या केससाठी वेगळा track करतो.
	deliveredOK := false

	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return false, fmt.Errorf("Document read error.")
		}
		for _, attr := range doc.Attributes {
			if _, ok := attr.(*tg.DocumentAttributeVideo); ok {
				isVideo = true
				break
			}
		}
		if !isVideo && strings.HasPrefix(strings.ToLower(doc.MimeType), "video/") {
			isVideo = true
		}

		if isVideo && wmEnabled {
			fileName := ""
			for _, attr := range doc.Attributes {
				if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
					fileName = fn.FileName
					break
				}
			}
			watermarkText := dbUser.Mobile
			if watermarkText == "" {
				watermarkText = dbUser.Email
			}
			// [NEW] Naav pn watermark madhe — mobile/email cha varchya oli var.
			if dbUser.FullName != "" {
				watermarkText = dbUser.FullName + "\n" + watermarkText
			}
			var logChatID, logAccessHash int64
			if config.ValueOf.LogChannelID != 0 {
				if logPeer, lpErr := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage); lpErr == nil && logPeer != nil {
					// [FIX] config.ValueOf.LogChannelID हा stripInt() मुळे "-100"
					// शिवायचा raw channel_id असतो. worker ला मात्र पूर्ण Bot-API
					// स्वरूपाचा chat_id ("-100"+channelID) लागतो — नाहीतर
					// warm_peer() मध्ये (abs(chat_id) - 1e12) करताना चुकीचा/
					// negative channel_id तयार होतो आणि PEER_ID_INVALID येतं.
					logIDStr := fmt.Sprintf("-100%d", logPeer.ChannelID)
					logChatID, _ = strconv.ParseInt(logIDStr, 10, 64)
					logAccessHash = logPeer.AccessHash
				}
			}
			uploaded, colabMsgID, colabLogMsgID, viaColab, skipWM, wmErr := deliverVideoToUser(
				botClient, course.ChannelID, messageID, fileName, watermarkText,
				targetChatID, progressMsgID, caption+"\n💧 Protected copy", logChatID, logAccessHash, silent, editProgress,
			)
			if wmErr != nil {
				// [FIX] आधी इथे खरं कारण (wmErr) पूर्णपणे टाकून दिलं जायचं
				// आणि सगळ्यांना एकच generic message दिसायचं — त्यामुळे
				// /predownload summary मध्ये ❌ Err असलेल्या सगळ्या lectures
				// साठी "Video process करताना error. पुन्हा प्रयत्न करा."
				// हेच एकच दिसायचं, खरं कारण (FLOOD_WAIT, timeout, worker
				// crash, इ.) कधीच कळायचं नाही. आता तेच पुढे पाठवतो.
				return false, fmt.Errorf("Video process करताना error: %s", wmErr.Error())
			}
			if skipWM {
				if wmEnabled {
					return false, fmt.Errorf("Watermark server is offline or busy — unwatermarked video send is blocked")
				}
				watermarked = false
				inputMedia = &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}}
				alertAdminsUnwatermarkedDelivery(ctx, dbUser, course, targetChatID)
			} else {
				watermarked = true
				if viaColab {
					sentMessageID = colabMsgID
					logChannelMsgID = colabLogMsgID
					deliveredOK = true // worker ने आधीच थेट student ला पाठवलंय — sentMessageID 0 असला तरी delivery यशस्वी झालीये
				} else {
					inputMedia = &tg.InputMediaUploadedDocument{File: uploaded, MimeType: doc.MimeType, Attributes: doc.Attributes}
				}
			}
		} else {
			inputMedia = &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}}
		}
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return false, fmt.Errorf("Photo read error.")
		}
		inputMedia = &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}}
	default:
		return false, fmt.Errorf("हा media type supported नाही.")
	}

	if watermarked {
		caption += "\n💧 Protected copy"
	}

	// [FIX] आधी आपण थेट student ला Noforwards:true सोबत पाठवायचो, आणि मग
	// तीच copy LOG_CHANNEL कडे forward करायचा प्रयत्न करायचो — पण Telegram
	// स्वतःच noforwards असलेला message परत forward करू देत नाही (अगदी
	// पाठवणाऱ्यालाही) — त्यामुळे "CHAT_FORWARDS_RESTRICTED" error यायचा आणि
	// backup कधीच व्हायचा नाही. आता उलट क्रम: आधी LOG_CHANNEL ला (unprotected
	// master copy म्हणून) पाठवतो, मग तिथूनच student ला protected म्हणून
	// forward करतो — हेच copy पुढच्या वेळीही fast-path साठी वापरता येतं.
	// [FIX] PDF/photo/इतर non-video documents "reference-copy" पद्धतीने
	// (download-reupload न करता, फक्त ID+AccessHash+FileReference वापरून)
	// पाठवले जातात — हा FileReference काही सेकंदातच शिळा (stale) होऊ शकतो
	// आणि Telegram तेव्हा "FILE_REFERENCE_EXPIRED" देतं. Video कधीच हा error
	// दाखवत नाही कारण तो नेहमी fresh bytes download+reupload करतो (किंवा
	// colab worker तेच करतो) — फक्त हा reference-copy मार्ग (PDF/photo/
	// skip-watermark video) यावर अवलंबून आहे. admin_stream.go मध्ये आधीच
	// याच class च्या error साठी जड fallback pipeline आहे (fastCopyMessage
	// + download-fallback) — इथे तितकं जड लागत नाही, फक्त message परत ताजं
	// fetch करून नवीन reference घेतला की एकदा retry पुरेसं आहे.
	refreshInputMedia := func() (tg.InputMediaClass, error) {
		res2, err2 := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: channelPeer,
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
		})
		if err2 != nil {
			return nil, err2
		}
		msgs2, ok := res2.(*tg.MessagesChannelMessages)
		if !ok || len(msgs2.Messages) == 0 {
			return nil, fmt.Errorf("re-fetch: message सापडला नाही")
		}
		m2, ok := msgs2.Messages[0].(*tg.Message)
		if !ok || m2.Media == nil {
			return nil, fmt.Errorf("re-fetch: media सापडला नाही")
		}
		switch mm := m2.Media.(type) {
		case *tg.MessageMediaDocument:
			d2, ok := mm.Document.(*tg.Document)
			if !ok {
				return nil, fmt.Errorf("re-fetch: document read error")
			}
			return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d2.ID, AccessHash: d2.AccessHash, FileReference: d2.FileReference}}, nil
		case *tg.MessageMediaPhoto:
			p2, ok := mm.Photo.(*tg.Photo)
			if !ok {
				return nil, fmt.Errorf("re-fetch: photo read error")
			}
			return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: p2.ID, AccessHash: p2.AccessHash, FileReference: p2.FileReference}}, nil
		default:
			return nil, fmt.Errorf("re-fetch: unsupported media type")
		}
	}
	isFileRefError := func(e error) bool {
		return e != nil && strings.Contains(strings.ToUpper(e.Error()), "FILE_REFERENCE")
	}

	if sentMessageID == 0 && inputMedia != nil {
		editProgress("📨 पाठवत आहोत...")

		if config.ValueOf.LogChannelID != 0 {
			if logPeer, lpErr := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage); lpErr == nil && logPeer != nil {
				logPeerInput := &tg.InputPeerChannel{ChannelID: logPeer.ChannelID, AccessHash: logPeer.AccessHash}
				// [FIX] कायम सारखाच RandomID (देखील retry मध्ये) — वरचं
				// deterministicRandomID doc-comment बघा.
				logRandomID := deterministicRandomID("log", dbUser.ID.Hex(), courseID.Hex(), strconv.Itoa(messageID))
				logSent, logErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
					Peer:     logPeerInput,
					Media:    inputMedia,
					Message:  caption,
					RandomID: logRandomID,
				})
				if logErr != nil && isFileRefError(logErr) {
					utils.Logger.Sugar().Warnw("catalog: FILE_REFERENCE error on log-channel upload — fresh reference घेऊन retry करतोय", "err", logErr, "messageID", messageID)
					if freshMedia, rfErr := refreshInputMedia(); rfErr == nil {
						inputMedia = freshMedia
						logSent, logErr = botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
							Peer: logPeerInput, Media: inputMedia, Message: caption, RandomID: logRandomID,
						})
					}
				}
				if logErr != nil {
					if waitSecs, isFlood := extractFloodWaitSeconds(logErr); isFlood {
						utils.Logger.Sugar().Warnw("catalog: log-channel upload failed with FLOOD_WAIT — aborting", "waitSecs", waitSecs)
						return false, logErr
					}
				}
				if logErr == nil {
					logMsgID := extractNewMessageID(toUpdates(logSent))
					if logMsgID != 0 {
						logChannelMsgID = logMsgID
						if silent {
							// Silent pre-download — LOG_CHANNEL मध्ये गेलं इतकंच
							// पुरेसं आहे, targetChatID ला काहीही पाठवायचं नाही.
							deliveredOK = true
						} else if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, logMsgID, targetChatID); fErr == nil {
							sentMessageID = extractNewMessageID(fwdUpdates)
						} else {
							utils.Logger.Sugar().Warnw("catalog: forward from log-channel to student failed", "err", fErr)
						}
					}
				} else {
					utils.Logger.Sugar().Warnw("catalog: log-channel upload failed, sending direct to student instead", "err", logErr)
				}
			}
		}

		// LOG_CHANNEL configure नसेल, किंवा वरचा मार्ग काही कारणाने जमला
		// नाही — तरी student ला video मिळायलाच हवं, थेट पाठवतो (backup/cache
		// न होता, आधीसारखंच). Silent मोडमध्ये मात्र targetChatID ला कधीच थेट
		// पाठवायचं नाही (तो user काही मागतच नाहीये) — LOG_CHANNEL मध्येच गेलं
		// नसेल तर silent pre-download साठी हे lecture फक्त अयशस्वी मानायचं.
		if sentMessageID == 0 && !silent {
			directRandomID := deterministicRandomID("direct", dbUser.ID.Hex(), courseID.Hex(), strconv.Itoa(messageID))
			sentMedia, sendErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:       toPeer,
				Media:      inputMedia,
				Message:    caption,
				Noforwards: true,
				RandomID:   directRandomID,
			})
			if sendErr != nil && isFileRefError(sendErr) {
				utils.Logger.Sugar().Warnw("catalog: FILE_REFERENCE error on direct send — fresh reference घेऊन retry करतोय", "err", sendErr, "messageID", messageID)
				if freshMedia, rfErr := refreshInputMedia(); rfErr == nil {
					inputMedia = freshMedia
					sentMedia, sendErr = botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
						Peer: toPeer, Media: inputMedia, Message: caption, Noforwards: true, RandomID: directRandomID,
					})
				}
			}
			if sendErr != nil {
				if waitSecs, isFlood := extractFloodWaitSeconds(sendErr); isFlood {
					utils.Logger.Sugar().Warnw("catalog: direct send failed with FLOOD_WAIT — aborting", "waitSecs", waitSecs)
					return false, sendErr
				}
				return false, fmt.Errorf("पाठवताना error: %s", sendErr.Error())
			}
			sentMessageID = extractNewMessageID(toUpdates(sentMedia))
		} else if sentMessageID == 0 && silent && !deliveredOK {
			return false, fmt.Errorf("silent pre-download: LOG_CHANNEL upload अयशस्वी")
		}
	}

	// ── DeliveredCache save — कुठलीही delivery (video/PDF/unwatermarked, log
	// channel सहित असो वा नसो) झाली की हे लक्षात ठेवतो. दोन उपयोग:
	//   1. LOG_CHANNEL master copy असेल (logChannelMsgID != 0) तर पुढच्या वेळी
	//      याच user ने हीच lecture परत मागितली तर नवीन watermark job / re-upload
	//      अजिबात न करता वरच्या "Cache hit" मार्गाने थेट forward होईल.
	//   2. कुठल्याही delivery साठी (logChannelMsgID 0 असला तरी) — folder-listing
	//      मध्ये या lecture वर "✅ आधीच पाहिलं/डाउनलोड केलं" mark दाखवता येतं.
	utils.Logger.Sugar().Infow("catalog: delivery finished — cache save check",
		"userID", dbUser.ID.Hex(), "courseID", courseID.Hex(), "messageID", messageID,
		"sentMessageID", sentMessageID, "logChannelMsgID", logChannelMsgID, "watermarked", watermarked, "deliveredOK", deliveredOK)
	if sentMessageID != 0 || deliveredOK {
		if cacheErr := db.UpsertDeliveredCache(dbUser.ID, courseID, messageID, logChannelMsgID, watermarked); cacheErr != nil {
			utils.Logger.Sugar().Warnw("catalog: UpsertDeliveredCache failed", "err", cacheErr)
		} else {
			utils.Logger.Sugar().Infow("catalog: UpsertDeliveredCache saved OK")
		}
	} else {
		utils.Logger.Sugar().Warnw("catalog: sentMessageID is 0 and delivery not confirmed — DeliveredCache NOT saved (tick/cache won't work for this delivery)")
	}

	if sentMessageID != 0 {
		_ = db.RecordSentLectureMsg(dbUser.ID, courseID, targetChatID, sentMessageID)
	}
	return true, nil
}

// handlePlayFolder — [NEW] संपूर्ण folder एका टॅपमध्ये user ला पाठवतो. प्रत्येक
// lecture साठी हुबेहूब तोच handlePlayLecture() call करतो (loop मध्ये) —
// त्यामुळे प्रत्येक lecture साठी वेगळा code लिहावा लागत नाही, आणि सगळे फायदे
// आपोआप मिळतात: cache-hit fast-path (आधीच पाठवलेलं लगेच log-channel वरून),
// per-user watermark (mobile/email), DeliveredCache save (✅ mark), इ.
// User ने delete केलं तरी परत request केल्यावर fast येतं (हेच handlePlayLecture
// च्या cache-hit मार्गामुळे) — त्यामुळे इथेही तेच आपोआप लागू होतं.
func handlePlayFolder(ctx *ext.Context, u *ext.Update, chatID int64, courseID, folderID primitive.ObjectID) {
	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		editCatalogMessage(ctx, u, "❌ तुमचा Telegram account link केलेला नाही.\n\n`/register <mobile_or_email>` वापरा.",
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		return
	}
	if blocked, until, reason := db.IsUserBlocked(dbUser.ID); blocked {
		editCatalogMessage(ctx, u, fmt.Sprintf("⛔ Account suspended.\n📅 Until: %s IST\n📝 Reason: %s",
			until.In(istLoc()).Format("02 Jan 2006, 3:04 PM"), reason),
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		return
	}
	hasAccess, _ := db.HasAccess(dbUser.ID, courseID)
	if !hasAccess {
		editCatalogMessage(ctx, u, "⛔ या course चा access नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		return
	}

	folderRef := "ungrouped"
	isUngrouped := folderID == primitive.NilObjectID
	if !isUngrouped {
		folderRef = folderID.Hex()
	}
	backRow := backOnlyRow("wc:folder:" + courseID.Hex() + ":" + folderRef + ":0")

	// renderFolderContentsView सारखीच matching logic — folder मधले सगळे
	// messageIDs (pagination शिवाय, पूर्ण यादी) काढतो.
	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	var msgIDs []int
	for _, f := range files {
		fo := db.FolderForSortOrder(folders, f.SortOrder)
		if isUngrouped {
			if fo != nil {
				continue
			}
		} else {
			if fo == nil || fo.ID != folderID {
				continue
			}
		}
		msgIDs = append(msgIDs, f.MessageID)
	}

	if len(msgIDs) == 0 {
		editCatalogMessage(ctx, u, "ℹ️ या folder मध्ये काही नाही.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}

	total := len(msgIDs)
	editCatalogMessage(ctx, u, fmt.Sprintf("📥 Folder मधले %d lectures पाठवायला सुरुवात करत आहोत...\n(एकावेळी एक — थोडा वेळ लागू शकतो)", total),
		&tg.ReplyInlineMarkup{Rows: backRow})

	go func() {
		sent, failed := 0, 0
		for i, msgID := range msgIDs {
			retry := true
			for retry {
				retry = false
				func() {
					defer func() {
						if r := recover(); r != nil {
							utils.Logger.Sugar().Errorw("handlePlayFolder: panic in single-lecture delivery recovered",
								"panic", r, "messageID", msgID)
							failed++
						}
					}()
					err := handlePlayLecture(ctx, u, chatID, courseID, msgID, true)
					if err != nil {
						if waitSecs, isFlood := extractFloodWaitSeconds(err); isFlood {
							utils.Logger.Sugar().Warnw("handlePlayFolder: FLOOD_WAIT hit, sleeping and retrying", "waitSecs", waitSecs)
							time.Sleep(time.Duration(waitSecs+2) * time.Second)
							retry = true
						} else {
							failed++
						}
					} else {
						sent++
					}
				}()
			}
			// Rate-limit टाळायला lectures मध्ये छोटा delay.
			if i < len(msgIDs)-1 {
				time.Sleep(3 * time.Second)
			}
		}

		summaryText := fmt.Sprintf("✅ *Folder Download पूर्ण!*\n📦 एकूण: %d\n📤 पाठवले: %d", total, sent)
		if failed > 0 {
			summaryText += fmt.Sprintf("\n❌ अयशस्वी: %d (यांच्यासाठी वेगळं try कर)", failed)
		}
		sumCtx, sumCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer sumCancel()
		toPeer := ctx.PeerStorage.GetInputPeerById(chatID)
		_, _ = ctx.Raw.MessagesSendMessage(sumCtx, &tg.MessagesSendMessageRequest{
			Peer: toPeer, Message: summaryText, RandomID: rand.Int63(),
			ReplyMarkup: &tg.ReplyInlineMarkup{Rows: backRow},
		})
	}()
}

// handlePlayCourse — [NEW] संपूर्ण course एका टॅपमध्ये user ला पाठवतो. प्रत्येक
// folder साठी आधी त्याचं naav plain text message म्हणून पाठवतो, मग त्या
// folder मधल्या सगळ्या files (video/PDF/image — कुठलाही media type) एका
// मागून एक पाठवतो — तेच handlePlayLecture() वापरून (cache-hit fast-path,
// watermark, DeliveredCache — सगळे फायदे handlePlayFolder सारखेच आपोआप
// मिळतात). शेवटी Ungrouped files (कुठल्याही folder मध्ये नसलेले) असतील तर
// तेही "📄 Ungrouped" नावाखाली त्याच पद्धतीने पाठवतो.
func handlePlayCourse(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID) {
	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		editCatalogMessage(ctx, u, "❌ तुमचा Telegram account link केलेला नाही.\n\n`/register <mobile_or_email>` वापरा.",
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		return
	}
	if blocked, until, reason := db.IsUserBlocked(dbUser.ID); blocked {
		editCatalogMessage(ctx, u, fmt.Sprintf("⛔ Account suspended.\n📅 Until: %s IST\n📝 Reason: %s",
			until.In(istLoc()).Format("02 Jan 2006, 3:04 PM"), reason),
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		return
	}
	hasAccess, _ := db.HasAccess(dbUser.ID, courseID)
	if !hasAccess {
		editCatalogMessage(ctx, u, "⛔ या course चा access नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		return
	}

	backRow := backOnlyRow("wc:course:" + courseID.Hex())

	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}

	// Folder-order राखत, प्रत्येक folder-ID खाली त्याचे messageIDs गोळा करतो.
	// Ungrouped साठी वेगळी bucket (शेवटी पाठवायला).
	type folderGroup struct {
		name   string
		msgIDs []int
	}
	groups := make([]*folderGroup, 0, len(folders)+1)
	groupByFolderID := make(map[primitive.ObjectID]*folderGroup, len(folders))
	for _, fo := range folders {
		g := &folderGroup{name: fo.Name}
		groups = append(groups, g)
		groupByFolderID[fo.ID] = g
	}
	var ungrouped folderGroup
	ungrouped.name = "Ungrouped"

	total := 0
	for _, f := range files {
		fo := db.FolderForSortOrder(folders, f.SortOrder)
		if fo == nil {
			ungrouped.msgIDs = append(ungrouped.msgIDs, f.MessageID)
		} else if g, ok := groupByFolderID[fo.ID]; ok {
			g.msgIDs = append(g.msgIDs, f.MessageID)
		} else {
			ungrouped.msgIDs = append(ungrouped.msgIDs, f.MessageID)
		}
		total++
	}
	if len(ungrouped.msgIDs) > 0 {
		groups = append(groups, &ungrouped)
	}

	if total == 0 {
		editCatalogMessage(ctx, u, "ℹ️ या course मध्ये अजून काही upload झालेलं नाही.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}

	// File lookup map बनवतो — MessageID वरून Title/FileName मिळवण्यासाठी
	fileMap := make(map[int]db.CachedFile, len(files))
	for _, f := range files {
		fileMap[f.MessageID] = f
	}

	editCatalogMessage(ctx, u, fmt.Sprintf("📥 संपूर्ण course पाठवायला सुरुवात करत आहोत...\n📦 एकूण: %d फाईल्स, %d folders\n(एकावेळी एक — वेळ लागू शकतो)", total, len(groups)),
		&tg.ReplyInlineMarkup{Rows: backRow})

	go func() {
		toPeer := ctx.PeerStorage.GetInputPeerById(chatID)
		sent, failed := 0, 0
		first := true

		type failedCourseItem struct {
			folderName string
			msgID      int
			title      string
			errText    string
		}
		var failedItems []failedCourseItem

		for _, g := range groups {
			if len(g.msgIDs) == 0 {
				continue
			}
			if !first {
				time.Sleep(2 * time.Second)
			}
			first = false
			nameCtx, nameCancel := context.WithTimeout(context.Background(), 10*time.Second)
			nameResp, nameErr := ctx.Raw.MessagesSendMessage(nameCtx, &tg.MessagesSendMessageRequest{
				Peer: toPeer, Message: "📁 " + g.name, RandomID: rand.Int63(),
			})
			nameCancel()

			if nameErr == nil {
				newMsgID := extractSentMessageID(nameResp)
				if newMsgID != 0 {
					pinCtx, pinCancel := context.WithTimeout(context.Background(), 10*time.Second)
					_, pinErr := ctx.Raw.MessagesUpdatePinnedMessage(pinCtx, &tg.MessagesUpdatePinnedMessageRequest{
						Peer:   toPeer,
						ID:     newMsgID,
						Silent: true,
					})
					pinCancel()
					if pinErr != nil {
						if waitSecs, isFlood := extractFloodWaitSeconds(pinErr); isFlood {
							utils.Logger.Sugar().Warnw("handlePlayCourse: pin FLOOD_WAIT — skipping pin for this folder",
								"chat_id", chatID, "msg_id", newMsgID, "wait_secs", waitSecs)
						} else {
							utils.Logger.Sugar().Warnw("handlePlayCourse: pin folder-name message failed (permission?)",
								"chat_id", chatID, "msg_id", newMsgID, "err", pinErr)
						}
					}
				} else {
					utils.Logger.Sugar().Warnw("handlePlayCourse: could not determine sent message ID — skipping pin",
						"chat_id", chatID, "resp_type", fmt.Sprintf("%T", nameResp))
				}
			} else {
				utils.Logger.Sugar().Warnw("handlePlayCourse: folder-name message send failed, skipping pin",
					"chat_id", chatID, "err", nameErr)
			}

			for i, msgID := range g.msgIDs {
				retry := true
				for retry {
					retry = false
					func() {
						defer func() {
							if r := recover(); r != nil {
								utils.Logger.Sugar().Errorw("handlePlayCourse: panic in single-lecture delivery recovered",
									"panic", r, "messageID", msgID)
								failed++
								title := fmt.Sprintf("Msg #%d", msgID)
								if cf, ok := fileMap[msgID]; ok {
									if cf.Title != "" {
										title = cf.Title
									} else if cf.FileName != "" {
										title = cf.FileName
									} else if cf.Caption != "" {
										title = cf.Caption
									}
								}
								failedItems = append(failedItems, failedCourseItem{
									folderName: g.name,
									msgID:      msgID,
									title:      title,
									errText:    fmt.Sprintf("panic: %v", r),
								})
							}
						}()
						err := handlePlayLecture(ctx, u, chatID, courseID, msgID, true)
						if err != nil {
							if waitSecs, isFlood := extractFloodWaitSeconds(err); isFlood {
								utils.Logger.Sugar().Warnw("handlePlayCourse: FLOOD_WAIT hit, sleeping and retrying", "waitSecs", waitSecs)
								time.Sleep(time.Duration(waitSecs+2) * time.Second)
								retry = true
							} else {
								failed++
								title := fmt.Sprintf("Msg #%d", msgID)
								if cf, ok := fileMap[msgID]; ok {
									if cf.Title != "" {
										title = cf.Title
									} else if cf.FileName != "" {
										title = cf.FileName
									} else if cf.Caption != "" {
										title = cf.Caption
									}
								}
								failedItems = append(failedItems, failedCourseItem{
									folderName: g.name,
									msgID:      msgID,
									title:      title,
									errText:    err.Error(),
								})
							}
						} else {
							sent++
						}
					}()
				}
				if i < len(g.msgIDs)-1 {
					time.Sleep(3 * time.Second)
				}
			}
		}

		summaryText := fmt.Sprintf("✅ *Course Download पूर्ण!*\n📦 एकूण: %d\n📤 पाठवले: %d", total, sent)
		if failed > 0 {
			summaryText += fmt.Sprintf("\n❌ अयशस्वी: %d", failed)
		}

		// ── Error summary formatting (Folder + Title/Caption + Full Error) ─────
		if len(failedItems) > 0 {
			var errLines []string
			currFolder := ""
			for _, item := range failedItems {
				if item.folderName != currFolder {
					currFolder = item.folderName
					errLines = append(errLines, fmt.Sprintf("\n📁 *Folder:* `%s`", currFolder))
				}
				errLines = append(errLines, fmt.Sprintf("• 📌 Msg `%d` — `%s`\n  └ ❌ `%s`", item.msgID, item.title, item.errText))
			}

			fullDetails := strings.Join(errLines, "\n")
			fullSummaryMsg := summaryText + "\n\n📋 *अयशस्वी फाईल्सचा रिपोर्ट:*" + fullDetails

			// Telegram 4096 character limit Check
			if len(fullSummaryMsg) <= 3800 {
				summaryText = fullSummaryMsg
			} else {
				// 3800 पात्रांपेक्षा जास्त किंवा खूप मोठे एरर्स असतील तर multiple msgs मध्ये पाठवतो
				summaryText += "\n\n📋 *अयशस्वी फाईल्सची माहिती खालीलप्रमाणे:*"
				sumCtx, sumCancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, _ = ctx.Raw.MessagesSendMessage(sumCtx, &tg.MessagesSendMessageRequest{
					Peer: toPeer, Message: summaryText, RandomID: rand.Int63(),
					ReplyMarkup: &tg.ReplyInlineMarkup{Rows: backRow},
				})
				sumCancel()

				// Chunk करून पाठवतो
				currChunk := ""
				for _, line := range errLines {
					if len(currChunk)+len(line)+2 > 3500 {
						cCtx, cCancel := context.WithTimeout(context.Background(), 10*time.Second)
						_, _ = ctx.Raw.MessagesSendMessage(cCtx, &tg.MessagesSendMessageRequest{
							Peer: toPeer, Message: currChunk, RandomID: rand.Int63(),
						})
						cCancel()
						time.Sleep(1 * time.Second)
						currChunk = line
					} else {
						if currChunk != "" {
							currChunk += "\n" + line
						} else {
							currChunk = line
						}
					}
				}
				if currChunk != "" {
					cCtx, cCancel := context.WithTimeout(context.Background(), 10*time.Second)
					_, _ = ctx.Raw.MessagesSendMessage(cCtx, &tg.MessagesSendMessageRequest{
						Peer: toPeer, Message: currChunk, RandomID: rand.Int63(),
					})
					cCancel()
				}
				return
			}
		}

		sumCtx, sumCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer sumCancel()
		_, _ = ctx.Raw.MessagesSendMessage(sumCtx, &tg.MessagesSendMessageRequest{
			Peer: toPeer, Message: summaryText, RandomID: rand.Int63(),
			ReplyMarkup: &tg.ReplyInlineMarkup{Rows: backRow},
		})
	}()
}

func isMessageNotModifiedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "message_not_modified") || strings.Contains(s, "not modified")
}

func renderPremiumMenuView(userID int64) (string, tg.ReplyMarkupClass) {
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, userID)

	statusStr := "❌ Not Connected (Login Required)"
	if doc, _ := db.GetUserBotSessionDocForUser(userID); doc != nil && doc.Session != "" && doc.Key == fmt.Sprintf("user_%d", userID) {
		statusStr = fmt.Sprintf("✅ Connected (%s)", doc.Phone)
	} else if isAdmin {
		if docAdmin, _ := db.GetUserBotSessionDoc(); docAdmin != nil && docAdmin.Session != "" {
			statusStr = fmt.Sprintf("⚙️ Admin System Userbot Connected (%s)", docAdmin.Phone)
		}
	}

	wm, wmOk := getWatermarkTextForUser(userID)
	if !wmOk || wm == "" {
		wm = "(Not Set)"
	}
	scID, scOk := getSendChannelIDForUser(userID)
	scStr := "(Direct Bot Chat)"
	if scOk && scID != 0 {
		scStr = fmt.Sprintf("`%d`", scID)
	}

	text := fmt.Sprintf(
		"⭐ *Premium Features & Userbot Tools*\n\n"+
			"👤 Userbot Status: %s\n"+
			"💧 Custom Watermark: `%s`\n"+
			"📢 Destination Channel: %s\n\n"+
			"खालीलपैकी पर्याय निवडा:",
		statusStr, wm, scStr,
	)

	rows := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "⬇️ Userbot Download", Data: []byte("wc:userbot_dl")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🔑 Connect Userbot Account", Data: []byte("wc:userbot_login")},
			&tg.KeyboardButtonCallback{Text: "ℹ️ Status", Data: []byte("wc:userbot_status")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📢 Set Destination Channel", Data: []byte("wc:userbot_sendchannel")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "💧 Set Watermark Text", Data: []byte("wc:userbot_watermark")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🗑️ Remove Watermark", Data: []byte("wc:userbot_rm_watermark")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🚪 Logout Userbot", Data: []byte("wc:userbot_logout")},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "⬅️ Back to Main Menu", Data: []byte("wc:root")},
		}},
	}
	return text, &tg.ReplyInlineMarkup{Rows: rows}
}

func renderUserbotStatusDetailView(userID int64) (string, tg.ReplyMarkupClass) {
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, userID)

	statusStr := "❌ Not Connected (Login Required)"
	phoneStr := "(None)"
	nameStr := "(Not Logged In)"

	doc, _ := db.GetUserBotSessionDocForUser(userID)
	if doc != nil && doc.Session != "" {
		statusStr = "✅ Connected & Active"
		phoneStr = doc.Phone
		nameStr = fmt.Sprintf("%s %s", doc.FirstName, doc.LastName)
		if doc.Username != "" {
			nameStr = fmt.Sprintf("%s (@%s)", nameStr, doc.Username)
		}
	} else if isAdmin {
		if docAdmin, _ := db.GetUserBotSessionDoc(); docAdmin != nil && docAdmin.Session != "" {
			statusStr = "⚙️ Admin System Userbot Active"
			phoneStr = docAdmin.Phone
			nameStr = fmt.Sprintf("%s %s", docAdmin.FirstName, docAdmin.LastName)
		}
	}

	wm, wmOk := getWatermarkTextForUser(userID)
	if !wmOk || wm == "" {
		wm = "(Not Set)"
	}
	scID, scOk := getSendChannelIDForUser(userID)
	scStr := "(Direct Bot Chat)"
	if scOk && scID != 0 {
		scStr = fmt.Sprintf("`%d`", scID)
	}

	text := fmt.Sprintf(
		"ℹ️ *Userbot & Settings Detailed Status*\n\n"+
			"👤 *Account Status:* %s\n"+
			"📞 *Phone:* `%s`\n"+
			"📛 *Account Name:* %s\n"+
			"🆔 *Telegram User ID:* `%d`\n\n"+
			"⚙️ *Your Settings:*\n"+
			"💧 *Watermark Text:* `%s`\n"+
			"📢 *Destination Channel:* %s\n"+
			"⚡ *Concurrency:* Parallel Enabled",
		statusStr, phoneStr, nameStr, userID, wm, scStr,
	)

	return text, &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "⬅️ Back to Userbot Menu", Data: []byte("wc:premium_menu")},
		}},
	}}
}