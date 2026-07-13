// internal/commands/register.go
//
// /register <mobile_or_email>
//
// दोन case:
//
//  1. Identifier आधीच website वर registered आहे (किंवा आधी कधीतरी बनलेलं
//     account) → पूर्वीसारखंच: admin ला /settg ready-made command पाठवतो,
//     admin copy-paste करून link करतो.
//
//  2. Identifier कुठेच सापडला नाही (पूर्ण नवीन user, website var kadhीच
//     account nahi) → आता website वर जायची गरज नाही — बॉट मध्येच पूर्ण
//     registration form (नाव + दुसरा contact detail) भरून घेतो, account
//     तयार करतो, आणि लगेच त्याच Telegram ID शी auto-link करतो (वेगळ्या
//     admin approval ची गरज नाही — Telegram identity स्वतःच पुरेशी आहे).
//     Website फक्त optional राहतो — कोणाला login/password वापरायचा असेल तेव्हाच.
package commands

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (m *command) LoadRegister(d dispatcher.Dispatcher) {
	log := m.log.Named("register")
	defer log.Sugar().Info("Loaded /register command")
	d.AddHandler(handlers.NewCommand("register", cmdRegister))

	// Group -3 — इतर सगळ्या handlers च्याही आधी चालतो (catalog.go च्या
	// gift-card catcher सारखाच pattern). Pending नसेल तर लगेच ContinueGroups.
	d.AddHandlerToGroup(handlers.NewMessage(nil, handleSelfRegisterReply), -3)
}

// /register <mobile_or_email>
func cmdRegister(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()

	// फक्त private chat मध्ये
	peerInfo := ctx.PeerStorage.GetPeerById(chatID)
	if peerInfo.Type != int(storage.TypeUser) {
		reply(ctx, u, "⚠️ /register फक्त bot शी private chat मध्ये वापरा.")
		return dispatcher.EndGroups
	}

	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"📋 *Register करा*\n\n"+
				"तुमचा mobile number किंवा email टाका:\n\n"+
				"`/register 9876543210`\n"+
				"किंवा\n"+
				"`/register yourname@email.com`\n\n"+
				"आधीच website वर account असेल तर तेच link होईल, नसेल तर आपण इथेच नवीन account बनवू.")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))

	// DB मध्ये user आहे का check करा
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil {
		reply(ctx, u, "❌ Error: "+err.Error())
		return dispatcher.EndGroups
	}

	if user == nil {
		startSelfRegister(ctx, u, chatID, identifier)
		return dispatcher.EndGroups
	}

	// आधीच linked आहे का?
	if user.TelegramID != 0 {
		if user.TelegramID == chatID {
			reply(ctx, u,
				"✅ तुमचा account आधीच linked आहे!\n\n"+
					"📚 Courses बघण्यासाठी: /courses\n\n"+
					"📥 Offline Download साठी `/dl <TOKEN>` वापरू शकता.\n"+
					"Token कसा मिळवायचा:\n"+
					"1. Website वर course उघडा\n"+
					"2. Video च्या खाली 📥 *Offline* button दाबा\n"+
					"3. मिळालेला token `/dl XXXXX` असा पाठवा")
		} else {
			reply(ctx, u,
				"⚠️ हा account दुसऱ्या Telegram ID शी linked आहे.\n"+
					"Admin शी संपर्क करा.")
		}
		return dispatcher.EndGroups
	}

	// ── Existing (website-registered) पण link नाही — जुनाच admin-approval flow ──
	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही. थोड्या वेळाने पुन्हा करा.")
		return dispatcher.EndGroups
	}

	adminIDs := []int64(config.ValueOf.AllowedUsers)
	if len(adminIDs) == 0 {
		reply(ctx, u, "❌ Admin configure केलेला नाही.")
		return dispatcher.EndGroups
	}

	settgCmd := fmt.Sprintf("/settg %s %d", identifier, chatID)
	adminMsg := fmt.Sprintf(
		"🔔 New Link Request\n\n"+
			"👤 User: %s\n"+
			"📛 Name: %s\n"+
			"📱 TG ID: %d\n\n"+
			"खाली copy करा आणि send करा:\n"+
			"%s",
		identifier,
		user.FullName,
		chatID,
		settgCmd,
	)

	for _, adminID := range adminIDs {
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     &tg.InputPeerUser{UserID: adminID},
			Message:  adminMsg,
			RandomID: mrand.Int63(),
		})
	}

	if adminEmail := config.ValueOf.AdminEmail; adminEmail != "" {
		go func() {
			_ = email.SendRegistrationRequest(adminEmail, identifier, user.FullName, chatID)
		}()
	}

	reply(ctx, u,
		"✅ *Request पाठवली!*\n\n"+
			"Admin तुमचा account लवकरच link करेल.\n"+
			"Link झाल्यावर `/dl` command वापरता येईल.\n\n"+
			"📥 *Offline Download कसे करायचे:*\n"+
			"1. Website वर course उघडा\n"+
			"2. Video च्या खाली 📥 *Offline* button दाबा\n"+
			"3. Token मिळेल — उदा. `48271`\n"+
			"4. इथे टाका: `/dl 48271`\n"+
			"5. Bot video पाठवेल — forward व save blocked असेल\n\n"+
			"⚠️ रोज फक्त *5 downloads* — midnight IST ला reset")

	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Self-registration (नवीन user, website वर कुठेच account नाही) — बॉट मध्येच फॉर्म
// ═══════════════════════════════════════════════════════════════════════════

type selfRegPhase string

const (
	regPhaseAskIdentifier selfRegPhase = "ask_identifier" // फक्त "Old User" button path साठी
	regPhaseFullName      selfRegPhase = "full_name"
	regPhaseMobile        selfRegPhase = "mobile"
	regPhaseEmail         selfRegPhase = "email"
)

type selfRegState struct {
	Identifier     string // आधीच माहीत असेल तर (typed /register command वरून) — नाहीतर रिकामा
	IsEmail        bool
	Phase          selfRegPhase
	FullName       string
	Mobile         string
	Email          string
	ResumeCourseID *primitive.ObjectID // "Buy → Gift Card" मधून आलं असेल तर — पूर्ण झाल्यावर तिथेच परत नेण्यासाठी
	ExpiresAt      time.Time
}

const selfRegTTL = 10 * time.Minute

var (
	selfRegMu      sync.Mutex
	selfRegPending = map[int64]*selfRegState{}
)

func setSelfRegPending(chatID int64, st *selfRegState) {
	st.ExpiresAt = time.Now().Add(selfRegTTL)
	selfRegMu.Lock()
	selfRegPending[chatID] = st
	selfRegMu.Unlock()
}

func getSelfRegPending(chatID int64) *selfRegState {
	selfRegMu.Lock()
	defer selfRegMu.Unlock()
	st, ok := selfRegPending[chatID]
	if !ok {
		return nil
	}
	if time.Now().After(st.ExpiresAt) {
		delete(selfRegPending, chatID)
		return nil
	}
	return st
}

func clearSelfRegPending(chatID int64) {
	selfRegMu.Lock()
	delete(selfRegPending, chatID)
	selfRegMu.Unlock()
}

func looksLikeEmail(s string) bool {
	return strings.Contains(s, "@")
}

// startSelfRegister — typed `/register <identifier>` वरून, ओळख आधीच माहीत आहे.
func startSelfRegister(ctx *ext.Context, u *ext.Update, chatID int64, identifier string) {
	if !db.GetSettingBool(db.SettingRegistrationOpen, true) {
		reply(ctx, u, "🚫 सध्या नवीन registrations बंद आहेत. काही वेळाने पुन्हा प्रयत्न करा.")
		return
	}
	setSelfRegPending(chatID, &selfRegState{
		Identifier: identifier,
		IsEmail:    looksLikeEmail(identifier),
		Phase:      regPhaseFullName,
	})
	reply(ctx, u,
		"📋 *नवीन Account बनवूया!*\n\n"+
			"हा mobile/email आधी कुठेच registered नाहीये — काळजी नको, आपण इथेच नवीन account बनवू.\n\n"+
			"1️⃣ तुमचं पूर्ण नाव सांगा:")
}

// startNewUserRegistration — "🆕 नवीन User" button वरून (Buy → Gift Card च्या आधी).
// ओळख (mobile/email) अजून माहीत नाही — नाव, मग mobile, मग email असं क्रमाने विचारतो.
func startNewUserRegistration(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID) {
	if !db.GetSettingBool(db.SettingRegistrationOpen, true) {
		editCatalogMessage(ctx, u, "🚫 सध्या नवीन registrations बंद आहेत. काही वेळाने पुन्हा प्रयत्न करा.",
			&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
		return
	}
	cid := courseID
	setSelfRegPending(chatID, &selfRegState{
		Phase:          regPhaseFullName,
		ResumeCourseID: &cid,
	})
	editCatalogMessage(ctx, u,
		"📋 *नवीन Account बनवूया!*\n\n1️⃣ तुमचं पूर्ण नाव सांगा:",
		&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
}

// startOldUserLink — "👤 आधीच Account आहे" button वरून. Mobile/email टाईप करायला सांगतो
// (command नाही, plain reply).
func startOldUserLink(ctx *ext.Context, u *ext.Update, chatID int64, courseID primitive.ObjectID) {
	cid := courseID
	setSelfRegPending(chatID, &selfRegState{
		Phase:          regPhaseAskIdentifier,
		ResumeCourseID: &cid,
	})
	editCatalogMessage(ctx, u,
		"👤 *Account Link करूया*\n\nतुमचा mobile number किंवा email ID टाका:",
		&tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:course:" + courseID.Hex())})
}

// handleSelfRegisterReply — plain-text catcher (gift-card flow सारखाच pattern), group -3.
func handleSelfRegisterReply(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	if msg == nil || strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return dispatcher.ContinueGroups
	}
	chatID := u.EffectiveUser().GetID()
	st := getSelfRegPending(chatID)
	if st == nil {
		return dispatcher.ContinueGroups
	}
	text := strings.TrimSpace(msg.Text)

	switch st.Phase {
	case regPhaseAskIdentifier:
		if text == "" {
			reply(ctx, u, "❌ रिकामं चालणार नाही. Mobile number किंवा email टाका:")
			return dispatcher.EndGroups
		}
		identifier := strings.ToLower(text)
		existing, err := db.GetUserByMobileOrEmail(identifier)
		if err != nil {
			reply(ctx, u, "❌ Error: "+err.Error())
			clearSelfRegPending(chatID)
			return dispatcher.EndGroups
		}
		if existing != nil {
			// आधीच website वर account आहे — जुनाच admin-approval flow.
			clearSelfRegPending(chatID)
			sendLinkRequestToAdmin(ctx, u, chatID, identifier, existing, st.ResumeCourseID)
			return dispatcher.EndGroups
		}
		// सापडला नाही — नवीन registration म्हणून पुढे चालू ठेवतो, हाच identifier वापरून.
		st.Identifier = identifier
		st.IsEmail = looksLikeEmail(identifier)
		st.Phase = regPhaseFullName
		setSelfRegPending(chatID, st)
		reply(ctx, u,
			"ℹ️ हा mobile/email आधी कुठेच registered नाहीये — नवीन account बनवूया.\n\n"+
				"1️⃣ तुमचं पूर्ण नाव सांगा:")
		return dispatcher.EndGroups

	case regPhaseFullName:
		if text == "" {
			reply(ctx, u, "❌ नाव रिकामं चालणार नाही. पुन्हा सांगा:")
			return dispatcher.EndGroups
		}
		st.FullName = text
		if st.Identifier != "" && st.IsEmail {
			st.Email = st.Identifier
			st.Phase = regPhaseMobile
			setSelfRegPending(chatID, st)
			reply(ctx, u, "2️⃣ Mobile number सांगा (नसेल तर - पाठवा):")
		} else if st.Identifier != "" && !st.IsEmail {
			st.Mobile = st.Identifier
			st.Phase = regPhaseEmail
			setSelfRegPending(chatID, st)
			reply(ctx, u, "2️⃣ Email address सांगा (नसेल तर - पाठवा):")
		} else {
			st.Phase = regPhaseMobile
			setSelfRegPending(chatID, st)
			reply(ctx, u, "2️⃣ Mobile number सांगा (नसेल तर - पाठवा):")
		}
		return dispatcher.EndGroups

	case regPhaseMobile:
		if text != "-" {
			st.Mobile = text
		}
		if st.Email != "" {
			// Identifier आधीच email होता, mobile वैकल्पिक विचारून झालं — आता पूर्ण.
			clearSelfRegPending(chatID)
			finishSelfRegister(ctx, u, chatID, st)
			return dispatcher.EndGroups
		}
		st.Phase = regPhaseEmail
		setSelfRegPending(chatID, st)
		reply(ctx, u, "3️⃣ Email address सांगा (नसेल तर - पाठवा):")
		return dispatcher.EndGroups

	case regPhaseEmail:
		if text != "-" {
			st.Email = strings.ToLower(text)
		}
		if st.Mobile == "" && st.Email == "" {
			reply(ctx, u, "❌ Mobile आणि Email दोन्ही रिकामं चालणार नाही — निदान एक तरी सांगा:")
			return dispatcher.EndGroups
		}
		clearSelfRegPending(chatID)
		finishSelfRegister(ctx, u, chatID, st)
		return dispatcher.EndGroups
	}

	clearSelfRegPending(chatID)
	return dispatcher.ContinueGroups
}

// sendLinkRequestToAdmin — website वर आधीच account असलेल्या (पण Telegram link नसलेल्या)
// user साठी — admin ला /settg ready-made command पाठवतो (typed-command flow सारखंच).
func sendLinkRequestToAdmin(ctx *ext.Context, u *ext.Update, chatID int64, identifier string, user *db.User, resumeCourseID *primitive.ObjectID) {
	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही. थोड्या वेळाने पुन्हा करा.")
		return
	}
	if user.TelegramID != 0 {
		if user.TelegramID == chatID {
			reply(ctx, u, "✅ तुमचा account आधीच linked आहे! /courses वापरून बघा.")
		} else {
			reply(ctx, u, "⚠️ हा account दुसऱ्या Telegram ID शी आधीच linked आहे. Admin शी संपर्क करा.")
		}
		return
	}

	adminIDs := []int64(config.ValueOf.AllowedUsers)
	settgCmd := fmt.Sprintf("/settg %s %d", identifier, chatID)
	adminMsg := fmt.Sprintf(
		"🔔 New Link Request (via Bot)\n\n👤 User: %s\n📛 Name: %s\n📱 TG ID: %d\n\nखाली copy करा आणि send करा:\n%s",
		identifier, user.FullName, chatID, settgCmd,
	)
	for _, adminID := range adminIDs {
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     &tg.InputPeerUser{UserID: adminID},
			Message:  adminMsg,
			RandomID: mrand.Int63(),
		})
	}
	if adminEmail := config.ValueOf.AdminEmail; adminEmail != "" {
		go func() {
			_ = email.SendRegistrationRequest(adminEmail, identifier, user.FullName, chatID)
		}()
	}

	var rows []tg.KeyboardButtonRow
	if resumeCourseID != nil {
		rows = backOnlyRow("wc:course:" + resumeCourseID.Hex())
	}
	editOrReply(ctx, u, chatID,
		"✅ *Request पाठवली!*\n\nAdmin तुमचा account लवकरच link करेल. Link झाल्यावर परत या course वर या — Gift Card option लगेच दिसेल.",
		rows)
}

func finishSelfRegister(ctx *ext.Context, u *ext.Update, chatID int64, st *selfRegState) {
	if existing, _ := db.GetUserByMobileOrEmail(firstNonEmpty(st.Email, st.Mobile)); existing != nil {
		reply(ctx, u, "⚠️ हा mobile/email आत्ताच दुसऱ्या कोणीतरी वापरला. कृपया परत प्रयत्न करा.")
		return
	}

	randomPass, err := randomPassword()
	if err != nil {
		reply(ctx, u, "❌ Server error, पुन्हा प्रयत्न करा.")
		return
	}
	hash, err := auth.HashPassword(randomPass)
	if err != nil {
		reply(ctx, u, "❌ Server error, पुन्हा प्रयत्न करा.")
		return
	}

	newUser, err := db.CreateUserFull("", st.FullName, st.Mobile, st.Email, hash)
	if err != nil {
		reply(ctx, u, "❌ Account तयार करताना error: "+err.Error())
		return
	}

	linked := db.SetUserTelegramID(newUser.ID, chatID) == nil

	if auth.NotifyAdmins != nil {
		go auth.NotifyAdmins(fmt.Sprintf(
			"🆕 New User Registered (via Bot)\n\nName: %s\nMobile: %s\nEmail: %s\nTelegram ID: %d",
			st.FullName, orDash(st.Mobile), orDash(st.Email), chatID,
		))
	}
	if adminEmail := config.ValueOf.AdminEmail; adminEmail != "" {
		go func() {
			_ = email.SendRegistrationRequest(adminEmail, firstNonEmpty(st.Email, st.Mobile), st.FullName, chatID)
		}()
	}

	if !linked {
		reply(ctx, u, "✅ Account तयार झालं, पण Telegram link करताना अडचण आली — admin ला कळवा.")
		return
	}

	if st.ResumeCourseID != nil {
		// लगेच त्याच course च्या Gift Card flow कडे परत नेतो — user ला परत क्लिक करावं लागत नाही.
		reply(ctx, u, "✅ *Account तयार झालं!* 🎉 आता Gift Card details देऊया:")
		startGiftCardFlow(ctx, u, chatID, *st.ResumeCourseID)
		return
	}

	reply(ctx, u, "✅ *Account तयार झालं!*\n\n👤 "+st.FullName+"\n\nआता तुम्ही थेट courses बघू आणि buy करू शकता 🎉")
	text, markup, vErr := renderCourseListView(chatID)
	if vErr == nil {
		ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// editOrReply — callback context असेल तर edit करतो, नाहीतर नवीन reply पाठवतो.
func editOrReply(ctx *ext.Context, u *ext.Update, chatID int64, text string, rows []tg.KeyboardButtonRow) {
	if u.CallbackQuery != nil {
		editCatalogMessage(ctx, u, text, &tg.ReplyInlineMarkup{Rows: rows})
		return
	}
	ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: &tg.ReplyInlineMarkup{Rows: rows}})
}

func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
