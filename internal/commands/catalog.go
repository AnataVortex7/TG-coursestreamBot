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
	"fmt"
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

// coursesButtonMarkup — /start च्या welcome messages वर लगेच "📚 Courses" बटण
// दाखवण्यासाठी (start.go वापरतो). दाबल्यावर wc:root कडेच जातो — म्हणजे
// /courses टाईप न करताही user ला लगेच course list दिसते.
func coursesButtonMarkup() tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📚 Courses पहा", Data: []byte("wc:root")},
		}},
	}}
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
		handlePlayLecture(ctx, u, chatID, courseID, msgID)
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

func answerCatalogError(ctx *ext.Context, u *ext.Update, err error) {
	editCatalogMessage(ctx, u, "❌ Error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Text: "⬅️ Courses", Data: []byte("wc:root")}}},
	}})
}

func editCatalogMessage(ctx *ext.Context, u *ext.Update, text string, markup tg.ReplyMarkupClass) {
	cbq := u.CallbackQuery
	if cbq == nil {
		// Callback context नाही (उदा. registration पूर्ण झाल्यावर plain-text
		// reply मधून थेट पुढच्या step कडे resume करताना) — edit करायला काही
		// नाही, त्यामुळे नवीन message म्हणून पाठवतो.
		_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
		return
	}
	chatID := u.EffectiveUser().GetID()
	_, err := ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
		ID:          cbq.MsgID,
		Message:     text,
		ReplyMarkup: markup,
	})
	if err != nil {
		// आधी हा error silently discard व्हायचा — म्हणजे edit fail झालं तरी
		// console वर काहीच दिसायचं नाही आणि screen वरही काही बदलायचं नाही
		// (button दाबल्यासारखं वाटायचं, पण पुढे काहीच व्हायचं नाही).
		// आता: 1) नेमकं कारण log होतं, 2) edit-in-place जमलं नाही तरी
		// खाली नवीन message म्हणून तेच content पाठवतो — user ला निदान
		// काहीतरी दिसेल.
		utils.Logger.Sugar().Errorw("catalog: EditMessage failed, sending fallback message",
			"chat_id", chatID, "msg_id", cbq.MsgID, "err", err)
		if _, sendErr := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup}); sendErr != nil {
			utils.Logger.Sugar().Errorw("catalog: fallback message send also failed", "chat_id", chatID, "err", sendErr)
		}
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
const lecturesPerPage = 12

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

func handlePlayLecture(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID, messageID int) {
	busyKey := fmt.Sprintf("%d:%d", chatID, messageID)
	if !lockPlay(busyKey) {
		if u.CallbackQuery != nil {
			_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
				QueryID: u.CallbackQuery.QueryID,
				Message: "⏳ आधीच पाठवत आहोत, थोडं थांबा...",
			})
		}
		return
	}
	defer unlockPlay(busyKey)

	if u.CallbackQuery != nil {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID})
	}

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

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		editCatalogMessage(ctx, u, "❌ Course सापडला नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
		return
	}

	backRow := backOnlyRow("wc:course:" + courseID.Hex())
	editCatalogMessage(ctx, u, "⏳ Lecture तयार करत आहोत...", &tg.ReplyInlineMarkup{Rows: backRow})

	// ── Cache hit — आधीच watermark लावलेली copy LOG_CHANNEL मध्ये आहे ─────
	if cached, cErr := db.GetDeliveredCache(dbUser.ID, courseID, messageID); cErr == nil && cached != nil {
		utils.Logger.Sugar().Infow("catalog: DeliveredCache hit — trying fast-path forward",
			"logChannelMsgID", cached.LogChannelMsgID, "chatID", chatID)
		if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, cached.LogChannelMsgID, chatID); fErr == nil {
			if newMsgID := extractNewMessageID(fwdUpdates); newMsgID != 0 {
				_ = db.RecordSentLectureMsg(dbUser.ID, courseID, chatID, newMsgID)
			}
			editCatalogMessage(ctx, u, "✅ लेक्चर खाली पाठवलं आहे 👇", &tg.ReplyInlineMarkup{Rows: backRow})
			return
		} else {
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

	botClient := MainBotClient
	if botClient == nil {
		editCatalogMessage(ctx, u, "❌ Bot सध्या तयार नाही. थोड्या वेळाने प्रयत्न करा.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}

	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, course.ChannelID)
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Channel access error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channelPeer,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
	})
	if err != nil {
		editCatalogMessage(ctx, u, "❌ Message fetch error: "+err.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		editCatalogMessage(ctx, u, "❌ Lecture सापडली नाही.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}
	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		editCatalogMessage(ctx, u, "❌ Media सापडला नाही.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
	}

	toPeer := ctx.PeerStorage.GetInputPeerById(chatID)
	editProgress := func(text string) { editCatalogMessage(ctx, u, text, &tg.ReplyInlineMarkup{Rows: backRow}) }

	var inputMedia tg.InputMediaClass
	isVideo := false
	watermarked := false
	wmEnabled := db.GetSettingBool(db.SettingOfflineWatermarkEnabled, true)

	// Lecture cha exact naav — तेच जे folder list मध्ये button वर दिसलं होतं
	// (actual file name — CachedFile.FileName), जेणेकरून user ला लगेच कळेल हा तोच video/file आहे.
	lectureName := course.Title
	if cf, cfErr := db.GetCachedFile(messageID, course.ChannelID); cfErr == nil && cf != nil {
		if cf.FileName != "" {
			lectureName = cf.FileName
		} else if cf.Title != "" {
			lectureName = cf.Title
		}
	} else if msg.Message != "" {
		lectureName = msg.Message
	}
	caption := "🎬 " + lectureName
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
			editCatalogMessage(ctx, u, "❌ Document read error.", &tg.ReplyInlineMarkup{Rows: backRow})
			return
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
				chatID, u.CallbackQuery.MsgID, caption+"\n💧 Protected copy", logChatID, logAccessHash, editProgress,
			)
			if wmErr != nil {
				editCatalogMessage(ctx, u, "❌ Video process करताना error. पुन्हा प्रयत्न करा.", &tg.ReplyInlineMarkup{Rows: backRow})
				return
			}
			if skipWM {
				watermarked = false
				inputMedia = &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}}
				alertAdminsUnwatermarkedDelivery(ctx, dbUser, course, chatID)
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
			editCatalogMessage(ctx, u, "❌ Photo read error.", &tg.ReplyInlineMarkup{Rows: backRow})
			return
		}
		inputMedia = &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}}
	default:
		editCatalogMessage(ctx, u, "❌ हा media type supported नाही.", &tg.ReplyInlineMarkup{Rows: backRow})
		return
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
	if sentMessageID == 0 && inputMedia != nil {
		editProgress("📨 पाठवत आहोत...")

		if config.ValueOf.LogChannelID != 0 {
			if logPeer, lpErr := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage); lpErr == nil && logPeer != nil {
				logPeerInput := &tg.InputPeerChannel{ChannelID: logPeer.ChannelID, AccessHash: logPeer.AccessHash}
				logSent, logErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
					Peer:     logPeerInput,
					Media:    inputMedia,
					Message:  caption,
					RandomID: rand.Int63(),
				})
				if logErr == nil {
					logMsgID := extractNewMessageID(toUpdates(logSent))
					if logMsgID != 0 {
						logChannelMsgID = logMsgID
						if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, logMsgID, chatID); fErr == nil {
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
		// न होता, आधीसारखंच).
		if sentMessageID == 0 {
			sentMedia, sendErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:       toPeer,
				Media:      inputMedia,
				Message:    caption,
				Noforwards: true,
				RandomID:   rand.Int63(),
			})
			if sendErr != nil {
				editCatalogMessage(ctx, u, "❌ पाठवताना error: "+sendErr.Error(), &tg.ReplyInlineMarkup{Rows: backRow})
				return
			}
			sentMessageID = extractNewMessageID(toUpdates(sentMedia))
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

	editCatalogMessage(ctx, u, "✅ लेक्चर खाली पाठवलं आहे 👇", &tg.ReplyInlineMarkup{Rows: backRow})

	if sentMessageID != 0 {
		_ = db.RecordSentLectureMsg(dbUser.ID, courseID, chatID, sentMessageID)
	}
}
