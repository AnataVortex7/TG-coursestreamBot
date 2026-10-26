// internal/commands/contact_gate.go
//
// Contact Gate — spam/misuse protection.
//
// ┌─ Registered + Telegram-linked user  → काहीच popup नाही, bot नेहमीसारखा चालतो.
// └─ Linked नसलेला user (/start किंवा कुठलाही पहिला message)
//
//	    │
//	    ▼
//	Popup: "Telegram Name + ID + Mobile (+ Email) share करायला Accept करा"
//	    ├─ ❌ Reject → "Accept शिवाय bot वापरता येणार नाही".
//	    │             पुढचा कुठलाही message → तोच popup परत.
//	    └─ ✅ Accept → "📱 Share my number" button (Telegram चा request-phone button)
//	             │
//	             ▼
//	      Contact येतो. contact.UserID == sender ID असेल तरच valid
//	      (दुसऱ्याचा contact forward केला तर reject).
//	             │
//	             ├─ DB मध्ये mobile सापडला
//	             │     ├─ Telegram link नाही   → auto-link (mobile verified आहे)
//	             │     │                          account ला email नसेल तर optional email विचारतो
//	             │     └─ दुसऱ्या Telegram शी linked → user ला सांगतो + admin ला notify
//	             │
//	             └─ mobile सापडला नाही → email विचारतो (optional, `-` = skip)
//	                   ├─ email दिला आणि तो एखाद्या जुन्या account चा आहे
//	                   │     → email verified नाही, म्हणून auto-link नाही;
//	                   │       जुना admin-approval flow (sendLinkRequestToAdmin)
//	                   └─ नाहीतर → नवीन account (verified mobile + Telegram name) + auto-link
//
// Group -6: सगळ्या जुन्या handlers (-4, -3, -2, -1, 0) च्या आधी चालतो.
package commands

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (m *command) LoadContactGate(d dispatcher.Dispatcher) {
	log := m.log.Named("contactgate")
	defer log.Sugar().Info("Loaded contact gate")

	d.AddHandlerToGroup(handlers.NewMessage(nil, contactGateMessage), -6)
	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("cg:"), contactGateCallback))
	go gateSweepStored() // aadhi verify zalele pan registration adhure — aata purna kara
}

// gateSweepStored — DB madhe verified mobile asleli pan registration/link na zaleli (gate_contacts) users cha
// registration aapoaap purna karto ani user + admin la kalavto. Bot start nantar ekda.
func gateSweepStored() {
	defer func() { _ = recover() }()
	time.Sleep(30 * time.Second)
	if MainBotClient == nil {
		return
	}
	for _, c := range db.ListUnregisteredVerifiedContacts(300) {
		gateCompleteStored(c)
		time.Sleep(1500 * time.Millisecond)
	}
}

func gateCompleteStored(c db.GateContact) {
	if linkedUser, _ := db.GetUserByTelegramID(c.TGID); linkedUser != nil {
		db.MarkContactRegistered(c.TGID)
		return
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = "Telegram User"
	}
	existing, _ := db.GetUserByMobileVariants(gateMobileVariants(c.Mobile))
	if existing != nil {
		if existing.TelegramID != 0 && existing.TelegramID != c.TGID {
			return // dusrya Telegram shi linked — manual
		}
		if existing.TelegramID == 0 {
			if err := db.SetUserTelegramID(existing.ID, c.TGID); err != nil {
				return
			}
		}
	} else {
		if !db.GetSettingBool(db.SettingRegistrationOpen, true) {
			return
		}
		pass, err := randomPassword()
		if err != nil {
			return
		}
		hash, err := auth.HashPassword(pass)
		if err != nil {
			return
		}
		nu, err := db.CreateUserFull("", name, c.Mobile, "", hash)
		if err != nil {
			return
		}
		if err := db.SetUserTelegramID(nu.ID, c.TGID); err != nil {
			return
		}
	}
	db.MarkContactRegistered(c.TGID)
	gateMarkLinked(c.TGID)
	_, _ = MainBotClient.CreateContext().SendMessage(c.TGID, &tg.MessagesSendMessageRequest{
		Message:     "✅ Tumcha registration purna zale! Aadhi tumcha number verify zala hota, mhanun account aata activate kela. 🎉\n\n📚 Courses baghayla /courses vapra.",
		ReplyMarkup: mainReplyKeyboardForUser(c.TGID),
	})
	if auth.NotifyAdmins != nil {
		first := ""
		if c.FirstMsg != "" {
			first = "\n\n💬 Pahila message: " + c.FirstMsg
		}
		auth.NotifyAdmins(fmt.Sprintf("✅ Adhura registration purna kele (verified mobile DB madhe hota)\n\nName: %s\nMobile: %s\nTelegram ID: %d%s", name, c.Mobile, c.TGID, first))
	}
}

// ─── State ───────────────────────────────────────────────────────────────────

type gatePhase string

const (
	gatePhaseNewEmail gatePhase = "new_email" // mobile verified, account नाही → optional email
	gatePhaseAddEmail gatePhase = "add_email" // जुना account link झाला, email नाही → optional email
)

type gateState struct {
	Phase     gatePhase
	Mobile    string
	FullName  string
	UserID    primitive.ObjectID // gatePhaseAddEmail साठी
	ExpiresAt time.Time
}

const (
	gateStateTTL       = 15 * time.Minute
	gateLinkedCacheTTL = 10 * time.Minute
	gatePopupCooldown  = 4 * time.Second
	gateLinkReqWindow  = 24 * time.Hour
)

var (
	gateMu        sync.Mutex
	gatePending   = map[int64]*gateState{}
	gateLinked    = map[int64]time.Time{} // positive cache: linked users
	gateLastPopup = map[int64]time.Time{} // popup flood control
	gateLinkReqAt = map[int64]time.Time{} // admin link request पाठवलेले users (24h pass-through)
	gateAccepted  = map[int64]time.Time{} // Accept dabavlele (number share baki) — popup parat parat nako
)

const gateAcceptedTTL = 30 * time.Minute

func gateIsAccepted(chatID int64) bool {
	gateMu.Lock()
	defer gateMu.Unlock()
	t, ok := gateAccepted[chatID]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(gateAccepted, chatID)
		return false
	}
	return true
}

func gateSetAccepted(chatID int64, on bool) {
	gateMu.Lock()
	if on {
		if len(gateAccepted) > 10000 {
			gateAccepted = map[int64]time.Time{}
		}
		gateAccepted[chatID] = time.Now().Add(gateAcceptedTTL)
	} else {
		delete(gateAccepted, chatID)
	}
	gateMu.Unlock()
}

func gateSetPending(chatID int64, st *gateState) {
	st.ExpiresAt = time.Now().Add(gateStateTTL)
	gateMu.Lock()
	gatePending[chatID] = st
	gateMu.Unlock()
}

func gateGetPending(chatID int64) *gateState {
	gateMu.Lock()
	defer gateMu.Unlock()
	st, ok := gatePending[chatID]
	if !ok {
		return nil
	}
	if time.Now().After(st.ExpiresAt) {
		delete(gatePending, chatID)
		return nil
	}
	return st
}

func gateClearPending(chatID int64) {
	gateMu.Lock()
	delete(gatePending, chatID)
	gateMu.Unlock()
}

func gateMarkLinked(chatID int64) {
	gateMu.Lock()
	if len(gateLinked) > 10000 {
		gateLinked = map[int64]time.Time{}
	}
	gateLinked[chatID] = time.Now().Add(gateLinkedCacheTTL)
	gateMu.Unlock()
}

// gateIsLinked — या Telegram ID ला website/bot account link आहे का?
// Linked users ना प्रत्येक message वर DB hit होऊ नये म्हणून 10 मिनिटं cache.
func gateIsLinked(chatID int64) (bool, error) {
	gateMu.Lock()
	if exp, ok := gateLinked[chatID]; ok && time.Now().Before(exp) {
		gateMu.Unlock()
		return true, nil
	}
	gateMu.Unlock()

	usr, err := db.GetUserByTelegramID(chatID)
	if err != nil {
		return false, err
	}
	if usr == nil {
		return false, nil
	}
	gateMarkLinked(chatID)
	return true, nil
}

// ForgetUserBotState — admin panel varun user permanent DELETE kelyavar call hoto.
// Gate cha "linked" positive-cache (10 min) ani baki in-memory state pusto, jenekarun
// to Telegram user pudhchya message la LAGECH nava (unregistered) mhanun treat hoil
// ani Verification popup -> number share -> registration flow punha suru hoil.
func ForgetUserBotState(tgID int64) {
	if tgID <= 0 {
		return
	}
	gateMu.Lock()
	delete(gateLinked, tgID)
	delete(gatePending, tgID)
	delete(gateLastPopup, tgID)
	delete(gateLinkReqAt, tgID)
	gateMu.Unlock()
	clearSelfRegPending(tgID)
}

// ─── Main message gate (group -6) ────────────────────────────────────────────

func contactGateMessage(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	user := u.EffectiveUser()
	if msg == nil || user == nil || u.EffectiveChat() == nil {
		return dispatcher.ContinueGroups
	}
	chatID := user.GetID()

	// फक्त bot सोबतचा private (1-to-1) chat. Channel / group / supergroup मध्ये (log channel,
	// send channel, userbot चे /dl commands, pre-download commands इ.) gate कधीच नाही —
	// तिथे sender (userbot/दुसरा user) चा ID user-type असतो, म्हणून फक्त sender चा type
	// बघून चालत नाही; chat स्वतः private आहे का हे तपासणं आवश्यक आहे.
	if !gateIsPrivateChat(ctx, u, chatID) {
		return dispatcher.ContinueGroups
	}
	peerInfo := ctx.PeerStorage.GetPeerById(chatID)
	if peerInfo.Type != int(storage.TypeUser) {
		return dispatcher.ContinueGroups
	}
	// Bot चे स्वतःचे messages
	if MainBotClient != nil && chatID == MainBotClient.Self.ID {
		return dispatcher.ContinueGroups
	}
	// Admin (AllowedUsers मध्ये स्पष्ट असलेले) — gate नाही
	if utils.Contains(config.ValueOf.AllowedUsers, chatID) {
		return dispatcher.ContinueGroups
	}

	// Registered + linked → popup नाही
	linked, err := gateIsLinked(chatID)
	if err != nil {
		// DB error वर user ला block करत नाही (fail-open), फक्त log.
		utils.Logger.Sugar().Warnw("contactgate: link check failed", "chat_id", chatID, "err", err)
		return dispatcher.ContinueGroups
	}
	if linked {
		return dispatcher.ContinueGroups
	}
	// Userbot access असलेले users (admin ने दिलेला) — gate नाही
	if db.HasUserbotAccess(chatID) {
		return dispatcher.ContinueGroups
	}

	// 0) Pahilyanda alelya (unregistered) user cha naav + Telegram ID + PAHILA message admin la — registration kela kiva nahi tari.
	go gateNotifyFirstContact(u, chatID)

	// 1) Contact share आला
	if c, ok := msg.Media.(*tg.MessageMediaContact); ok {
		return gateHandleContact(ctx, u, chatID, c)
	}

	txt := strings.TrimSpace(msg.Text)

	// 2) Commands: /register, /myid आणि website deep-links gate मधून जाऊ देतो
	if strings.HasPrefix(txt, "/") {
		fields := strings.Fields(txt)
		cmd := strings.ToLower(fields[0])
		if i := strings.Index(cmd, "@"); i > 0 {
			cmd = cmd[:i]
		}
		// [CHANGED] /register (जुना manual form) आणि dl_ deep-link आता गेटमधून जात नाहीत —
		// account तयार/linked होईपर्यंत प्रत्येक message ला तोच Verification popup येतो.
		// फक्त /myid आणि website चे totp_ (login link) / ct_ (admin contact) deep-links चालतात.
		if cmd == "/myid" {
			return dispatcher.ContinueGroups
		}
		if cmd == "/start" && len(fields) > 1 {
			p := fields[1]
			// [SECURITY] totp_ (website login link) is NO LONGER passed through for
			// unlinked users: the deep-link token is handed to whoever knows an account's
			// mobile/email, so letting it link a Telegram ID without phone verification
			// allowed account takeover. Unlinked users must pass the contact gate
			// (verified phone-number share) first.
			if strings.HasPrefix(p, "ct_") {
				return dispatcher.ContinueGroups
			}
		}
		gateClearPending(chatID) // command आली म्हणजे अर्धवट email step सोडून दिला
	} else if st := gateGetPending(chatID); st != nil && txt != "" {
		// 3) Email step चं उत्तर
		return gateHandlePending(ctx, u, chatID, st, txt)
	}

	// [CHANGED] जुन्या /register manual-form flow ला pass-through बंद — unlinked user ला
	// तो form भरता येत नाही; आधी Verification (Accept → number share) पूर्ण करावं लागतं.
	gateAbortOldRegister(chatID)

	// Admin link request आधीच पाठवलेला (verified mobile नंतर) → 24h pass-through
	gateMu.Lock()
	until, hasReq := gateLinkReqAt[chatID]
	gateMu.Unlock()
	if hasReq && time.Now().Before(until) {
		return dispatcher.ContinueGroups
	}

	// 4) Accept zalyavar user ne number share na karta kahi lihile => same popup nahi, fakt share-button reminder.
	if gateIsAccepted(chatID) {
		if gatePopupAllowed(chatID) {
			extra := ""
			if gateLooksLikeMobile(txt) {
				extra = "\n\n🔒 Number type kelela chalat nahi — verification sathi khalcha button vapra."
			}
			_, _ = ctx.Reply(u,
				ext.ReplyTextString("📱 Registration purna karnyasathi khalcha '📱 Share my number' button daba."+extra),
				&ext.ReplyOpts{Markup: gateShareKeyboard()})
		}
		return dispatcher.EndGroups
	}

	// 5) Popup
	gateSendPopup(ctx, u, chatID)
	return dispatcher.EndGroups
}

// gateLooksLikeMobile — user ne number type kela (7-15 digits).
func gateLooksLikeMobile(s string) bool {
	n := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			n++
		case r == '+' || r == ' ' || r == '-':
		default:
			return false
		}
	}
	return n >= 7 && n <= 15
}

// gateDescribeMsg — user cha pahila message (text kiva media prakar) admin sathi.
func gateDescribeMsg(msg *tg.Message) string {
	if t := strings.TrimSpace(msg.Message); t != "" {
		r := []rune(t)
		if len(r) > 600 {
			t = string(r[:600]) + "…"
		}
		return t
	}
	switch msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		return "[photo]"
	case *tg.MessageMediaDocument:
		return "[file/document/video]"
	case *tg.MessageMediaContact:
		return "[contact share]"
	case *tg.MessageMediaGeo, *tg.MessageMediaVenue:
		return "[location]"
	case nil:
		return "[empty]"
	}
	return "[media]"
}

// gateNotifyFirstContact — fakt pahilyanda (DB madhe nond nasel tevha) admin la pathavto.
func gateNotifyFirstContact(u *ext.Update, chatID int64) {
	defer func() { _ = recover() }()
	if auth.NotifyAdmins == nil || u.EffectiveMessage == nil {
		return
	}
	tu := u.EffectiveUser()
	name := strings.TrimSpace(tu.FirstName + " " + tu.LastName)
	if name == "" {
		name = "Telegram User"
	}
	first := gateDescribeMsg(u.EffectiveMessage.Message)
	isNew, err := db.RecordFirstContact(chatID, name, tu.Username, first)
	if err != nil || !isNew {
		return
	}
	uname := "—"
	if tu.Username != "" {
		uname = "@" + tu.Username
	}
	auth.NotifyAdmins(fmt.Sprintf(
		"👋 Navin user ne bot suru kela\n\n👤 Name: %s\n🔗 Username: %s\n🆔 Telegram ID: %d\n\n💬 Pahila message:\n%s\n\n⏳ Registration: pending (Accept / number share baki)",
		name, uname, chatID, first))
}

// gateIsPrivateChat — true फक्त जेव्हा message bot च्या private chat मध्ये आला असेल
// (chat ID == sender user ID आणि chat peer type = user). Channel/group मध्ये false.
func gateIsPrivateChat(ctx *ext.Context, u *ext.Update, senderID int64) bool {
	chat := u.EffectiveChat()
	if chat == nil {
		return false
	}
	cid := chat.GetID()
	if cid != senderID {
		return false
	}
	if config.ValueOf.LogChannelID != 0 && (cid == config.ValueOf.LogChannelID || -cid == config.ValueOf.LogChannelID) {
		return false
	}
	return ctx.PeerStorage.GetPeerById(cid).Type == int(storage.TypeUser)
}

// ─── Popup ───────────────────────────────────────────────────────────────────

// gatePopupAllowed — popup flood control (4 सेकंदात एकच). true = आता पाठवायला हरकत नाही.
func gatePopupAllowed(chatID int64) bool {
	gateMu.Lock()
	defer gateMu.Unlock()
	if last, ok := gateLastPopup[chatID]; ok && time.Since(last) < gatePopupCooldown {
		return false
	}
	if len(gateLastPopup) > 10000 {
		gateLastPopup = map[int64]time.Time{}
	}
	gateLastPopup[chatID] = time.Now()
	return true
}

const gatePopupText = "🔐 Verification आवश्यक\n\n" +
	"Spam आणि misuse टाळण्यासाठी हा bot वापरायला तुमचा Telegram account तुमच्या mobile number शी जोडावा लागतो.\n\n" +
	"आम्ही घेऊ:\n" +
	"• Telegram नाव आणि ID\n" +
	"• Mobile number (Telegram चा contact share button वापरून)\n\n" +
	"हे वापरायला तुम्ही Accept करता का?"

func gatePopupMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "✅ Accept", Data: []byte("cg:accept")},
		&tg.KeyboardButtonCallback{Text: "❌ Reject", Data: []byte("cg:reject")},
	}}}}
}

func gateSendPopup(ctx *ext.Context, u *ext.Update, chatID int64) {
	if !gatePopupAllowed(chatID) {
		return // flood control — spam केला तरी popup ची रांग लागत नाही
	}
	_, _ = ctx.Reply(u, ext.ReplyTextString(gatePopupText), &ext.ReplyOpts{Markup: gatePopupMarkup()})
}

// gateAbortOldRegister — जुन्या /register manual-form ची अर्धवट state असेल तर पुसतो
// (unlinked user ने तो form पुढे चालवू नये).
func gateAbortOldRegister(chatID int64) {
	if getSelfRegPending(chatID) != nil {
		clearSelfRegPending(chatID)
	}
}

// gateBlockCallback — inline बटणांच्या (wc:, menu:, cmd:) callbacks साठी गेट. Unlinked user ने जुन्या
// message वरचं बटण दाबलं (उदा. course list → manual form) तरी काम न होता Verification popup येतो.
// true = block केलं, caller ने काहीच करू नये.
func gateBlockCallback(ctx *ext.Context, u *ext.Update) bool {
	user := u.EffectiveUser()
	if user == nil {
		return false
	}
	chatID := user.GetID()
	if utils.Contains(config.ValueOf.AllowedUsers, chatID) {
		return false
	}
	if MainBotClient != nil && chatID == MainBotClient.Self.ID {
		return false
	}
	if linked, err := gateIsLinked(chatID); err != nil || linked {
		return false // DB error वर block नाही (fail-open)
	}
	if db.HasUserbotAccess(chatID) {
		return false
	}
	gateMu.Lock()
	until, hasReq := gateLinkReqAt[chatID]
	gateMu.Unlock()
	if hasReq && time.Now().Before(until) {
		return false
	}
	if cbq := u.CallbackQuery; cbq != nil {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			QueryID: cbq.QueryID, Message: "🔐 आधी Verification पूर्ण करा", Alert: true,
		})
	}
	gateAbortOldRegister(chatID)
	if gatePopupAllowed(chatID) {
		_, _ = ctx.SendMessage(chatID, &tg.MessagesSendMessageRequest{
			Message: gatePopupText, ReplyMarkup: gatePopupMarkup(),
		})
	}
	return true
}

func gateShareKeyboard() tg.ReplyMarkupClass {
	return &tg.ReplyKeyboardMarkup{
		Resize:    true,
		SingleUse: true,
		Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonRequestPhone{Text: "📱 Share my number"},
		}}},
	}
}

// gateSkipKeyboard — email step साठी "⏭ Skip" बटण (reply keyboard, SingleUse). बटण दाबलं की
// तेच लेबल text म्हणून येतं आणि gateHandlePending त्याला skip मानतो — user email step मध्ये अडकत नाही.
const gateSkipLabel = "⏭ Skip"

func gateSkipKeyboard() tg.ReplyMarkupClass {
	return &tg.ReplyKeyboardMarkup{
		Resize:    true,
		SingleUse: true,
		Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButton{Text: gateSkipLabel},
		}}},
	}
}

// ─── Accept / Reject callbacks ───────────────────────────────────────────────

func contactGateCallback(ctx *ext.Context, u *ext.Update) error {
	cbq := u.CallbackQuery
	if cbq == nil || u.EffectiveUser() == nil {
		return dispatcher.ContinueGroups
	}
	_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: cbq.QueryID})
	chatID := u.EffectiveUser().GetID()

	switch string(cbq.Data) {
	case "cg:accept":
		if linked, _ := gateIsLinked(chatID); linked {
			editCatalogMessage(ctx, u, "✅ तुमचा account आधीच verified आहे.", nil)
			return dispatcher.EndGroups
		}
		gateSetAccepted(chatID, true)
		editCatalogMessage(ctx, u, "✅ Accept केलं. आता खालचं '📱 Share my number' बटण दाबा.", nil)
		_, _ = ctx.SendMessage(chatID, &tg.MessagesSendMessageRequest{
			Message:     "👇 फक्त तुमचा स्वतःचा number share करा:",
			ReplyMarkup: gateShareKeyboard(),
		})

	case "cg:reject":
		gateSetAccepted(chatID, false)
		gateClearPending(chatID)
		gateMu.Lock()
		delete(gateLastPopup, chatID) // पुढच्या message वर popup लगेच परत यावा
		gateMu.Unlock()
		editCatalogMessage(ctx, u,
			"❌ तुम्ही Reject केलं.\n\nAccept केल्याशिवाय हा bot वापरता येणार नाही. कुठलाही message पाठवा — verification परत विचारलं जाईल.",
			nil)
	}
	return dispatcher.EndGroups
}

// ─── Contact received ────────────────────────────────────────────────────────

func gateHandleContact(ctx *ext.Context, u *ext.Update, chatID int64, c *tg.MessageMediaContact) error {
	// महत्त्वाचं: दुसऱ्याचा contact forward करून fake verification होऊ नये.
	if c.UserID != chatID {
		_, _ = ctx.Reply(u,
			ext.ReplyTextString("⚠️ हा तुमचा स्वतःचा number नाही. कृपया खालचं '📱 Share my number' बटण वापरा — contact forward करू नका."),
			&ext.ReplyOpts{Markup: gateShareKeyboard()})
		return dispatcher.EndGroups
	}

	mobile := gateNormalizeMobile(c.PhoneNumber)
	if mobile == "" {
		reply(ctx, u, "❌ Number वाचता आला नाही. पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}

	fullName := strings.TrimSpace(c.FirstName + " " + c.LastName)
	if fullName == "" {
		tu := u.EffectiveUser()
		fullName = strings.TrimSpace(tu.FirstName + " " + tu.LastName)
	}
	if fullName == "" {
		fullName = "Telegram User"
	}

	// Verified mobile lagech DB madhe — registration adhura rahila tari harvnar nahi.
	db.SaveVerifiedContact(chatID, mobile, fullName)
	gateSetAccepted(chatID, false)

	existing, err := db.GetUserByMobileVariants(gateMobileVariants(mobile))
	if err != nil {
		reply(ctx, u, "❌ Server error, थोड्या वेळाने पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}

	// ── A) Mobile आधीच DB मध्ये आहे ──────────────────────────────────────────
	if existing != nil {
		return gateLinkExisting(ctx, u, chatID, existing, mobile)
	}

	// ── B) Mobile नवीन → नवीन account बनवायचं आहे ────────────────────────────
	if !db.GetSettingBool(db.SettingRegistrationOpen, true) {
		_, _ = ctx.Reply(u,
			ext.ReplyTextString("🚫 सध्या नवीन registrations बंद आहेत. काही वेळाने पुन्हा प्रयत्न करा."),
			&ext.ReplyOpts{Markup: &tg.ReplyKeyboardHide{}})
		return dispatcher.EndGroups
	}
	// Email nako — verified mobile + Telegram naav var lagech registration + auto-link.
	finishSelfRegister(ctx, u, chatID, &selfRegState{FullName: fullName, Mobile: mobile})
	if linked, _ := gateIsLinked(chatID); linked {
		db.MarkContactRegistered(chatID)
		_, _ = ctx.Reply(u, ext.ReplyTextString("👇 Menu"), &ext.ReplyOpts{Markup: mainReplyKeyboardForUser(chatID)})
	}
	return dispatcher.EndGroups
}

func gateLinkExisting(ctx *ext.Context, u *ext.Update, chatID int64, existing *db.User, mobile string) error {
	switch {
	case existing.TelegramID == chatID:
		gateMarkLinked(chatID)
		gateFinish(ctx, u, chatID, "✅ तुमचा account आधीच linked आहे.")

	case existing.TelegramID != 0:
		// दुसऱ्या Telegram account शी linked
		_, _ = ctx.Reply(u,
			ext.ReplyTextString("⚠️ हा mobile number दुसऱ्या Telegram account शी आधीच linked आहे.\n\n"+
				"ज्या Telegram account वरून आधी link केलं होतं त्यातून bot वापरा. बदलायचं असेल तर Admin शी संपर्क करा."),
			&ext.ReplyOpts{Markup: &tg.ReplyKeyboardHide{}})
		if auth.NotifyAdmins != nil {
			go auth.NotifyAdmins(fmt.Sprintf(
				"⚠️ Link attempt (different Telegram)\n\nMobile: %s\nAccount: %s\nलिंक्ड TG ID: %d\nप्रयत्न करणारा TG ID: %d",
				mobile, existing.FullName, existing.TelegramID, chatID))
		}

	default:
		// Mobile verified आहे + Telegram link नाही → auto-link
		if err := db.SetUserTelegramID(existing.ID, chatID); err != nil {
			if err == db.ErrTelegramIDInUse {
				gateMarkLinked(chatID)
				gateFinish(ctx, u, chatID, "✅ तुमचा Telegram account आधीच linked आहे.")
				return dispatcher.EndGroups
			}
			reply(ctx, u, "❌ Link करताना error आली: "+err.Error())
			return dispatcher.EndGroups
		}
		gateMarkLinked(chatID)
		if auth.NotifyAdmins != nil {
			go auth.NotifyAdmins(fmt.Sprintf(
				"🔗 Auto-linked (mobile verified)\n\nName: %s\nMobile: %s\nTelegram ID: %d",
				existing.FullName, mobile, chatID))
		}
		db.MarkContactRegistered(chatID)
		gateFinish(ctx, u, chatID, "✅ Account link झालं! 🎉")
	}
	return dispatcher.EndGroups
}

// ─── Email step reply ────────────────────────────────────────────────────────

func gateHandlePending(ctx *ext.Context, u *ext.Update, chatID int64, st *gateState, text string) error {
	text = strings.TrimSpace(text)
	skip := text == "-" || strings.EqualFold(text, "skip") || text == gateSkipLabel

	emailAddr := ""
	if !skip {
		emailAddr = strings.ToLower(text)
		if !gateLooksLikeEmail(emailAddr) {
			// Verification step अजून पूर्ण झालेला नाही — user ने जुना prompt delete केला असला
			// किंवा काहीतरी वेगळं लिहिलं तरी, पुन्हा स्पष्ट prompt + Skip बटण देतो.
			gateSetPending(chatID, st) // 15 min चा वेळ पुन्हा सुरू
			_, _ = ctx.Reply(u,
				ext.ReplyTextString("🔐 Verification अजून पूर्ण झालेलं नाही.\n\n"+
					"📧 तुमचा Email पाठवा, किंवा Email नको असेल तर खालचं ⏭ Skip बटण दाबा."),
				&ext.ReplyOpts{Markup: gateSkipKeyboard()})
			return dispatcher.EndGroups
		}
	}
	gateClearPending(chatID)

	switch st.Phase {
	case gatePhaseAddEmail:
		if emailAddr != "" {
			if err := db.SetUserEmailIfEmpty(st.UserID, emailAddr); err != nil {
				reply(ctx, u, "⚠️ Email सेव्ह झाला नाही: "+err.Error())
			} else {
				reply(ctx, u, "✅ Email जोडला.")
			}
		}
		gateFinish(ctx, u, chatID, "✅ Verification पूर्ण! 🎉")

	case gatePhaseNewEmail:
		if emailAddr != "" {
			byEmail, err := db.GetUserByEmailExact(emailAddr)
			if err != nil {
				reply(ctx, u, "❌ Server error, थोड्या वेळाने पुन्हा प्रयत्न करा.")
				return dispatcher.EndGroups
			}
			if byEmail != nil {
				// Email verified नाही → कोणीही दुसऱ्याचा email टाकून त्याचे courses
				// हडपू शकेल. म्हणून auto-link नाही; admin approval flow.
				if byEmail.TelegramID == 0 {
					gateMu.Lock()
					gateLinkReqAt[chatID] = time.Now().Add(gateLinkReqWindow)
					gateMu.Unlock()
				}
				sendLinkRequestToAdmin(ctx, u, chatID, emailAddr, byEmail, nil)
				return dispatcher.EndGroups
			}
		}
		finishSelfRegister(ctx, u, chatID, &selfRegState{
			FullName: st.FullName,
			Mobile:   st.Mobile,
			Email:    emailAddr,
		})
		if linked, _ := gateIsLinked(chatID); linked {
			_, _ = ctx.Reply(u, ext.ReplyTextString("👇 Menu"), &ext.ReplyOpts{Markup: mainReplyKeyboardForUser(chatID)})
		}
	}
	return dispatcher.EndGroups
}

// gateFinish — link झाल्यावर main menu keyboard + course list.
func gateFinish(ctx *ext.Context, u *ext.Update, chatID int64, headline string) {
	_, _ = ctx.Reply(u, ext.ReplyTextString(headline), &ext.ReplyOpts{Markup: mainReplyKeyboardForUser(chatID)})
	if text, markup, err := renderCourseListView(chatID); err == nil {
		_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
	}
	go func() {
		time.Sleep(4 * time.Second)
		MaybePromptAutoDelivery(ctx, chatID)
	}()
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// gateNormalizeMobile — Telegram contact चा number (उदा. "919876543210") →
// DB format: भारतीय number असेल तर 10 digits ("9876543210"), इतर देशांचे full digits.
func gateNormalizeMobile(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		return d[2:]
	}
	if len(d) < 7 {
		return ""
	}
	return d
}

// gateMobileVariants — DB मध्ये जुना data वेगळ्या format मध्ये असेल तरी सापडावा.
func gateMobileVariants(m string) []string {
	if len(m) == 10 {
		return []string{m, "91" + m, "+91" + m, "0" + m}
	}
	return []string{m, "+" + m}
}

func gateLooksLikeEmail(s string) bool {
	if len(s) < 5 || len(s) > 254 || strings.ContainsAny(s, " \t\n") {
		return false
	}
	at := strings.Index(s, "@")
	if at <= 0 || at != strings.LastIndex(s, "@") {
		return false
	}
	domain := s[at+1:]
	dot := strings.LastIndex(domain, ".")
	return dot > 0 && dot < len(domain)-1
}
