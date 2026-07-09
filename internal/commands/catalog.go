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

func handleCatalogCallback(ctx *ext.Context, u *ext.Update) error {
	data := strings.TrimPrefix(string(u.CallbackQuery.Data), "wc:")
	parts := strings.Split(data, ":")
	if len(parts) == 0 {
		return dispatcher.EndGroups
	}
	chatID := u.EffectiveUser().GetID()

	switch parts[0] {
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
		text, markup, err := renderFolderContentsView(chatID, courseID, folderID)
		if err != nil {
			answerCatalogError(ctx, u, err)
			return dispatcher.EndGroups
		}
		editCatalogMessage(ctx, u, text, markup)

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
		return
	}
	chatID := u.EffectiveUser().GetID()
	_, _ = ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
		ID:          cbq.MsgID,
		Message:     text,
		ReplyMarkup: markup,
	})
	_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: cbq.QueryID})
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
			&tg.KeyboardButtonCallback{Text: "📁 " + fo.Name, Data: []byte("wc:folder:" + courseID.Hex() + ":" + fo.ID.Hex())},
		}})
	}
	if hasUngrouped {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📁 Ungrouped", Data: []byte("wc:folder:" + courseID.Hex() + ":ungrouped")},
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

func renderFolderContentsView(chatID int64, courseID, folderID primitive.ObjectID) (string, tg.ReplyMarkupClass, error) {
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

	folderName := "Ungrouped"
	var rows []tg.KeyboardButtonRow
	isUngrouped := folderID == primitive.NilObjectID

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
		title := f.Title
		if title == "" {
			title = f.FileName
		}
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "▶️ " + title, Data: []byte(fmt.Sprintf("wc:play:%s:%d", courseID.Hex(), f.MessageID))},
		}})
	}
	if len(rows) == 0 {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "ℹ️ या folder मध्ये काही नाही", Data: []byte("wc:course:" + courseID.Hex())},
		}})
	}
	rows = append(rows, backOnlyRow("wc:course:"+courseID.Hex())...)
	return fmt.Sprintf("📂 *%s*\n\nलेक्चर निवडा:", folderName), &tg.ReplyInlineMarkup{Rows: rows}, nil
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
		editCatalogMessage(ctx, u, "❌ आधी `/register <mobile_or_email>` करा, मग Gift Card payment submit करता येईल.",
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
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
		if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, cached.LogChannelMsgID, chatID); fErr == nil {
			if newMsgID := extractNewMessageID(fwdUpdates); newMsgID != 0 {
				_ = db.RecordSentLectureMsg(dbUser.ID, courseID, chatID, newMsgID)
			}
			editCatalogMessage(ctx, u, "✅ लेक्चर खाली पाठवलं आहे 👇", &tg.ReplyInlineMarkup{Rows: backRow})
			return
		}
		// Cache forward fail झालं (log channel मधून message delete झाला असेल) —
		// खालच्या fresh-delivery मार्गावर पडतो.
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
	// (CachedFile.Title), जेणेकरून user ला लगेच कळेल हा तोच video आहे.
	lectureName := course.Title
	if cf, cfErr := db.GetCachedFile(messageID, course.ChannelID); cfErr == nil && cf != nil {
		if cf.Title != "" {
			lectureName = cf.Title
		} else if cf.FileName != "" {
			lectureName = cf.FileName
		}
	} else if msg.Message != "" {
		lectureName = msg.Message
	}
	caption := "🎬 " + lectureName
	sentMessageID := 0

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
			uploaded, colabMsgID, viaColab, skipWM, wmErr := deliverVideoToUser(
				botClient, course.ChannelID, messageID, fileName, watermarkText,
				chatID, u.CallbackQuery.MsgID, caption+"\n💧 Protected copy", editProgress,
			)
			if wmErr != nil {
				editCatalogMessage(ctx, u, "❌ Video process करताना error. पुन्हा प्रयत्न करा.", &tg.ReplyInlineMarkup{Rows: backRow})
				return
			}
			if skipWM {
				watermarked = false
				inputMedia = &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}}
			} else {
				watermarked = true
				if viaColab {
					sentMessageID = colabMsgID
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

	if sentMessageID == 0 && inputMedia != nil {
		editProgress("📨 पाठवत आहोत...")
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
		if updates, ok := sentMedia.(*tg.Updates); ok {
			for _, upd := range updates.Updates {
				if m, ok := upd.(*tg.UpdateMessageID); ok {
					sentMessageID = m.ID
					break
				}
			}
		}
	}

	editCatalogMessage(ctx, u, "✅ लेक्चर खाली पाठवलं आहे 👇", &tg.ReplyInlineMarkup{Rows: backRow})

	if sentMessageID != 0 {
		_ = db.RecordSentLectureMsg(dbUser.ID, courseID, chatID, sentMessageID)
	}

	// ── LOG_CHANNEL मध्ये backup + cache record (पुढच्या वेळी fast-path) ────
	if sentMessageID != 0 && config.ValueOf.LogChannelID != 0 {
		go func() {
			updates, fErr := utils.ForwardMessages(ctx, chatID, config.ValueOf.LogChannelID, sentMessageID)
			if fErr != nil {
				utils.Logger.Sugar().Warnw("catalog: log-channel cache backup failed", "err", fErr)
				return
			}
			logMsgID := 0
			for _, upd := range updates.Updates {
				if m, ok := upd.(*tg.UpdateNewChannelMessage); ok {
					logMsgID = m.Message.GetID()
					break
				}
			}
			if logMsgID != 0 {
				_ = db.UpsertDeliveredCache(dbUser.ID, courseID, messageID, logMsgID, watermarked)
			}
		}()
	}
}
