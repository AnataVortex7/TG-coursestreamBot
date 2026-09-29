// internal/commands/menu.go
//
// /cmd → button-based admin menu (/start सोडून सर्व admin commands आता buttons
// मधून). Category-wise organized (Users, Course, Userbot, Sessions, Stream
// Settings, Colab, Guard, Linking, Stats).
//
// 3 प्रकारचे commands:
//   view    — button दाबताच direct result (existing cmd* function reuse)
//   toggle  — button दाबताच action + confirm (existing cmd* function reuse)
//   flow    — 1 किंवा जास्त "steps" चालतात, क्रमाने. प्रत्येक step एकतर:
//               select — numbered list दाखवून index मागतो
//               input  — मोकळा प्रश्न विचारतो
//             सगळे steps पूर्ण झाले की collected args जोडून original cmd*
//             function call होतो.
//
// महत्त्वाचं: existing cmd* functions (admin.go / admin_stream.go / guard.go /
// dl.go) मध्ये **कोणताही बदल नाही**. reply() helper (admin.go) callback-aware
// केला आहे — त्यामुळे तेच function command-line आणि button दोन्हीकडून चालतं.
//
// Flow पूर्ण होताना — admin च्या plain-text reply चा EffectiveMessage.Text
// तात्पुरता "/<key> <args...>" असा rewrite करून तोच original cmd* function
// call केला जातो (args() helper तसाच वाचतो).

package commands

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

// ═══════════════════════════════════════════════════════════════════════════
// Step definitions — select (numbered list) किंवा input (मोकळा प्रश्न)
// ═══════════════════════════════════════════════════════════════════════════

type stepKind int

const (
	stepSelect stepKind = iota
	stepInput
)

type cmdStep struct {
	kind   stepKind
	prompt string                             // select: list चा header | input: प्रश्न
	listFn func() ([]listItem, string, error) // select साठी — (items, empty-msg, err)
}

func inputStep(prompt string) cmdStep { return cmdStep{kind: stepInput, prompt: prompt} }

func selectStep(label string, listFn func() ([]listItem, string, error)) cmdStep {
	return cmdStep{kind: stepSelect, prompt: label, listFn: listFn}
}

// ═══════════════════════════════════════════════════════════════════════════
// Pending state — per-admin, in-memory (bot restart झाल्यास जातं, ठीक आहे)
// ═══════════════════════════════════════════════════════════════════════════

type pendingAction struct {
	key         string         // menuRegistry key
	currentStep cmdStep        // admin आत्ता कशाला उत्तर देतोय
	remaining   []cmdStep      // पुढचे steps
	collected   []string       // आत्तापर्यंत मिळालेले args (select→id, input→raw text)
	indexMap    map[int]string // currentStep select असेल तरच वापरला जातो
	expiresAt   time.Time
}

const pendingTTL = 2 * time.Minute

var (
	pendingMu sync.Mutex
	pending   = map[int64]*pendingAction{}
)

// ─── Userbot login — custom multi-round-trip flow ──────────────────────────
// SendCode/VerifyCode server-side state (phoneHash, 2FA) carry करतात, त्यामुळे
// हा generic step-engine (जो फक्त plain text collect करतो) मध्ये बसत नाही —
// वेगळा, dedicated state map वापरला आहे.

type loginPhase string

const (
	loginPhasePhone    loginPhase = "phone"
	loginPhaseCode     loginPhase = "code"
	loginPhasePassword loginPhase = "password"
)

type loginState struct {
	phase     loginPhase
	phone     string
	phoneHash string
	expiresAt time.Time
}

const loginTTL = 5 * time.Minute

var (
	userbotLoginMu      sync.Mutex
	userbotLoginByAdmin = map[int64]*loginState{}
)

func setUserbotLogin(adminID int64, s *loginState) {
	s.expiresAt = time.Now().Add(loginTTL)
	userbotLoginMu.Lock()
	userbotLoginByAdmin[adminID] = s
	userbotLoginMu.Unlock()
}

func getUserbotLogin(adminID int64) *loginState {
	userbotLoginMu.Lock()
	defer userbotLoginMu.Unlock()
	s, ok := userbotLoginByAdmin[adminID]
	if !ok {
		return nil
	}
	if time.Now().After(s.expiresAt) {
		delete(userbotLoginByAdmin, adminID)
		return nil
	}
	return s
}

func clearUserbotLogin(adminID int64) {
	userbotLoginMu.Lock()
	delete(userbotLoginByAdmin, adminID)
	userbotLoginMu.Unlock()
}

func setPending(adminID int64, p *pendingAction) {
	p.expiresAt = time.Now().Add(pendingTTL)
	pendingMu.Lock()
	pending[adminID] = p
	pendingMu.Unlock()
}

func getPending(adminID int64) *pendingAction {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pending[adminID]
	if !ok {
		return nil
	}
	if time.Now().After(p.expiresAt) {
		delete(pending, adminID)
		return nil
	}
	return p
}

func clearPending(adminID int64) {
	pendingMu.Lock()
	delete(pending, adminID)
	pendingMu.Unlock()
}

// ═══════════════════════════════════════════════════════════════════════════
// Command registry
// ═══════════════════════════════════════════════════════════════════════════

type cmdKind int

const (
	kindView cmdKind = iota
	kindToggle
	kindFlow
)

type listItem struct {
	display string // यादीत दाखवायचं
	id      string // action साठी actual identifier (multi-value असेल तर space ने जोडलेलं)
}

type menuCmd struct {
	key   string
	label string
	kind  cmdKind
	fn    func(*ext.Context, *ext.Update) error // view/toggle/flow-finalize — existing cmd* function
	steps []cmdStep                             // kindFlow साठी — क्रमाने
}

var menuRegistry = map[string]menuCmd{}

func registerCmd(c menuCmd) { menuRegistry[c.key] = c }

// ═══════════════════════════════════════════════════════════════════════════
// Categories
// ═══════════════════════════════════════════════════════════════════════════

type category struct {
	key     string
	label   string
	cmdKeys []string
}

var categories = []category{
	{"course", "📚 Course Management", []string{
		"listcourses", "addcourse", "delcourse", "grantaccess", "revokeaccess",
		"predownload", "userbotdl", "stopuserbotdl", "setdlimit",
		"addvideo", "syncchannel", "regstatus", "regopen", "regclose",
		"offlinewmstatus", "offlinewmon", "offlinewmoff", "setpaymentmode",
	}},
	{"userbot", "🤖 Userbot & Downloads", []string{
		"userbotstatus", "userbotlogin", "userbotlogout", "grantuserbot", "adminstream",
		"userbotdl", "predownload", "copychannel", "stopuserbotdl",
		"showwatermark", "setwatermark", "clearwatermark",
		"showsendchannel", "setsendchannel", "clearsendchannel",
		"setuserbotlog", "clearuserbotlog",
		"showthumbnail", "setthumbnail", "clearthumbnail",
		"listreplace", "setreplace", "delreplace", "clearreplace",
	}},
	{"users", "👥 Users Management", []string{
		"listusers", "adduser", "deluser", "grantaccess", "revokeaccess",
		"userinfo", "suspicious", "blockuser", "unblockuser", "settg", "unlinktg",
	}},
	{"sessions", "📱 Active Sessions", []string{"sessions", "kill", "killsession"}},
	{"stream", "⚙️ Stream & Watermark Settings", []string{
		"listreplace", "setreplace", "delreplace", "clearreplace",
		"showsendchannel", "setsendchannel", "clearsendchannel",
		"setuserbotlog", "clearuserbotlog",
		"showthumbnail", "setthumbnail", "clearthumbnail",
		"showwatermark", "setwatermark", "clearwatermark",
	}},
	{"colab", "☁️ Colab Worker", []string{"colabstatus", "colabstart", "colabstop"}},
	{"guard", "🛡️ Guard & Security", []string{"suspicious", "userinfo", "blockuser", "unblockuser"}},
	{"linking", "🔗 Account Linking", []string{"settg", "unlinktg"}},
	{"stats", "📊 Bot Analytics & Stats", []string{"stats"}},
}

func findCategory(key string) *category {
	for i := range categories {
		if categories[i].key == key {
			return &categories[i]
		}
	}
	return nil
}

func catKeyOf(cmdKey string) string {
	for _, c := range categories {
		for _, k := range c.cmdKeys {
			if k == cmdKey {
				return c.key
			}
		}
	}
	return ""
}

// ═══════════════════════════════════════════════════════════════════════════
// Select-step list sources
// ═══════════════════════════════════════════════════════════════════════════

func listUsersItems() ([]listItem, string, error) {
	users, err := db.ListUsers()
	if err != nil {
		return nil, "", err
	}
	if len(users) == 0 {
		return nil, "कोणतेही users नाहीत.", nil
	}
	items := make([]listItem, 0, len(users))
	for _, usr := range users {
		name := usr.FullName
		if name == "" {
			name = usr.Username
		}
		if name == "" {
			name = "User"
		}
		contact := usr.Mobile
		if contact == "" {
			contact = usr.Email
		}

		display := name
		if contact != "" {
			display = fmt.Sprintf("%s (%s)", name, contact)
		} else if usr.TelegramID != 0 {
			display = fmt.Sprintf("%s (TG: %d)", name, usr.TelegramID)
		}

		id := usr.Mobile
		if id == "" {
			id = usr.Email
		}
		if id == "" {
			id = usr.Username
		}
		if id == "" && usr.TelegramID != 0 {
			id = fmt.Sprintf("%d", usr.TelegramID)
		}

		items = append(items, listItem{display: display, id: id})
	}
	return items, "", nil
}

func listCoursesItems() ([]listItem, string, error) {
	courses, err := db.ListAllCourses()
	if err != nil {
		return nil, "", err
	}
	if len(courses) == 0 {
		return nil, "कोणतेही courses नाहीत.", nil
	}
	items := make([]listItem, 0, len(courses))
	for _, c := range courses {
		items = append(items, listItem{display: c.Title, id: c.ID.Hex()})
	}
	return items, "", nil
}

// listAccessItems — सर्व users चे सर्व active access grants, "username — course title"
// स्वरूपात. id = "username courseIDHex" (2 tokens, space ने वेगळे — cmdRevokeAccess
// ला थेट a[0]=username, a[1]=courseID असं मिळतं).
func listAccessItems() ([]listItem, string, error) {
	users, err := db.ListUsers()
	if err != nil {
		return nil, "", err
	}
	courses, err := db.ListAllCourses()
	if err != nil {
		return nil, "", err
	}
	titleByID := make(map[string]string, len(courses))
	for _, c := range courses {
		titleByID[c.ID.Hex()] = c.Title
	}
	var items []listItem
	for _, usr := range users {
		accessList, err := db.ActiveAccessForUser(usr.ID)
		if err != nil {
			continue
		}
		for _, a := range accessList {
			title := titleByID[a.CourseID.Hex()]
			if title == "" {
				title = a.CourseID.Hex()
			}
			items = append(items, listItem{
				display: fmt.Sprintf("%s — %s", usr.Username, title),
				id:      fmt.Sprintf("%s %s", usr.Username, a.CourseID.Hex()),
			})
		}
	}
	if len(items) == 0 {
		return nil, "सध्या कोणताही access grant नाही.", nil
	}
	return items, "", nil
}

func listActiveSessionsItems() ([]listItem, string, error) {
	sessions, err := db.ListActiveSessions()
	if err != nil {
		return nil, "", err
	}
	if len(sessions) == 0 {
		return nil, "कोणतेही active sessions नाहीत.", nil
	}
	items := make([]listItem, 0, len(sessions))
	for _, s := range sessions {
		uname := "unknown"
		if user, _ := db.GetUserByID(s.UserID); user != nil {
			uname = user.Username
		}
		items = append(items, listItem{
			display: fmt.Sprintf("%s — IP: %s", uname, s.IP),
			id:      uname,
		})
	}
	return items, "", nil
}

func listReplaceItems() ([]listItem, string, error) {
	var items []listItem
	captionReplaceStore.Range(func(k, v interface{}) bool {
		items = append(items, listItem{
			display: fmt.Sprintf("%s → %s", k.(string), v.(string)),
			id:      k.(string),
		})
		return true
	})
	if len(items) == 0 {
		return nil, "कोणतेही replace rules नाहीत.", nil
	}
	return items, "", nil
}

func listBlockedUsersItems() ([]listItem, string, error) {
	users, err := db.ListUsers()
	if err != nil {
		return nil, "", err
	}
	var items []listItem
	for _, usr := range users {
		blocked, until, reason := db.IsUserBlocked(usr.ID)
		if blocked {
			items = append(items, listItem{
				display: fmt.Sprintf("%s (until %s) — %s", usr.Username, until.Format("02 Jan 2006"), reason),
				id:      usr.Username,
			})
		}
	}
	if len(items) == 0 {
		return nil, "सध्या कोणतेही blocked users नाहीत.", nil
	}
	return items, "", nil
}

// setthumbnail बटणाने text-flow ने होत नाही (photo reply लागतो) — फक्त सूचना.
func cmdShowSetThumbnailHelp(ctx *ext.Context, u *ext.Update) error {
	reply(ctx, u, "📸 Thumbnail set करायला एखाद्या *photo* ला reply करून `/setthumbnail` टाईप करा.\n\n(हे बटणातून करता येत नाही — actual photo लागतो.)")
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Userbot — Status / Login / Logout
// ═══════════════════════════════════════════════════════════════════════════

func cmdUserbotStatusView(ctx *ext.Context, u *ext.Update) error {
	if UserbotStatus == nil {
		reply(ctx, u, "❌ Userbot bridge अजून ready नाही (bot restart झाला का ते बघा).")
		return dispatcher.EndGroups
	}
	running, firstName, lastName, username, phone, userID := UserbotStatus()
	if !running {
		reply(ctx, u, "🔴 Userbot सध्या login नाही.\n\nLogin करायला 🔑 Userbot Login वापरा.")
		return dispatcher.EndGroups
	}
	name := strings.TrimSpace(firstName + " " + lastName)
	uname := "—"
	if username != "" {
		uname = "@" + username
	}
	reply(ctx, u, fmt.Sprintf(
		"🟢 Userbot Active\n\n👤 Name: %s\n%s\n📱 Phone: %s\n🆔 User ID: %d",
		name, uname, phone, userID,
	))
	return dispatcher.EndGroups
}

func cmdUserbotLogout(ctx *ext.Context, u *ext.Update) error {
	if UserbotStatus == nil || UserbotStop == nil {
		reply(ctx, u, "❌ Userbot bridge अजून ready नाही (bot restart झाला का ते बघा).")
		return dispatcher.EndGroups
	}
	running, _, _, _, _, _ := UserbotStatus()
	if !running {
		reply(ctx, u, "⚠️ Userbot आधीच logged out आहे.")
		return dispatcher.EndGroups
	}
	UserbotStop()
	_ = db.DeleteUserBotSession()
	reply(ctx, u, "✅ Userbot logout केला — session delete झाला.\n\nपुन्हा login करायला 🔑 Userbot Login वापरा.")
	return dispatcher.EndGroups
}

func cmdUserbotLoginStart(ctx *ext.Context, u *ext.Update) error {
	if UserbotStatus == nil || UserbotSendCode == nil {
		editCatalogMessage(ctx, u, "❌ Userbot bridge अजून ready नाही (bot restart झाला का ते बघा).", premiumCancelMarkup())
		return dispatcher.EndGroups
	}
	senderID := u.EffectiveUser().GetID()
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	if isAdmin {
		running, _, _, username, _, _ := UserbotStatus()
		if running {
			editCatalogMessage(ctx, u, fmt.Sprintf("⚠️ Admin Userbot आधीच login आहे (@%s).\n\nआधी 🚪 Userbot Logout करा, मग नवीन account login करा.", username), premiumCancelMarkup())
			return dispatcher.EndGroups
		}
	} else {
		doc, _ := db.GetUserBotSessionDocForUser(senderID)
		if doc != nil && doc.Session != "" {
			editCatalogMessage(ctx, u, fmt.Sprintf("⚠️ तुमचे Userbot Account आधीच login आहे (%s).\n\nआधी 🚪 Userbot Logout करा, मग नवीन account login करा.", doc.Phone), premiumCancelMarkup())
			return dispatcher.EndGroups
		}
	}

	setUserbotLogin(senderID, &loginState{phase: loginPhasePhone})
	editCatalogMessage(ctx, u, "📱 *Connect Userbot Account*\n\nतुमचा Mobile number टाका (देशाच्या code सोबत, उदा: `+919876543210`):", premiumCancelMarkup())
	return dispatcher.EndGroups
}

// handleUserbotLoginReply — phone → OTP → (गरज लागल्यास) 2FA password, क्रमाने.
func handleUserbotLoginReply(ctx *ext.Context, u *ext.Update, adminID int64, ls *loginState, text string) error {
	if UserbotSendCode == nil || UserbotVerifyCode == nil {
		clearUserbotLogin(adminID)
		reply(ctx, u, "❌ Userbot bridge अजून ready नाही (bot restart झाला का ते बघा).")
		return dispatcher.EndGroups
	}

	switch ls.phase {
	case loginPhasePhone:
		phone := text
		hash, err := UserbotSendCode(phone)
		if err != nil {
			clearUserbotLogin(adminID)
			reply(ctx, u, "❌ "+err.Error())
			return dispatcher.EndGroups
		}
		setUserbotLogin(adminID, &loginState{phase: loginPhaseCode, phone: phone, phoneHash: hash})
		reply(ctx, u, "✅ OTP पाठवला!\n\n🔢 Telegram वर आलेला OTP code टाका:\n\n⚠️ *टीप:* Telegram code block करू नये म्हणून code मध्ये space किंवा dash देऊन टाका. (उदा: `9-9-0-8-7` किंवा `9 9 9 9 9`)")
		return dispatcher.EndGroups

	case loginPhaseCode:
		sessionStr, firstName, lastName, username, phoneOut, userID, err := UserbotVerifyCode(ls.phone, ls.phoneHash, text, "")
		if err != nil {
			if err.Error() == "SESSION_PASSWORD_NEEDED" {
				setUserbotLogin(adminID, &loginState{phase: loginPhasePassword, phone: ls.phone, phoneHash: ls.phoneHash})
				reply(ctx, u, "🔒 या account ला 2FA आहे — Password टाका:")
				return dispatcher.EndGroups
			}
			clearUserbotLogin(adminID)
			reply(ctx, u, "❌ "+err.Error()+"\n\nपुन्हा सुरू करायला 🔑 Userbot Login वापरा.")
			return dispatcher.EndGroups
		}
		finishUserbotLogin(ctx, u, adminID, sessionStr, firstName, lastName, username, phoneOut, userID)
		return dispatcher.EndGroups

	case loginPhasePassword:
		sessionStr, firstName, lastName, username, phoneOut, userID, err := UserbotVerifyCode(ls.phone, ls.phoneHash, "", text)
		if err != nil {
			reply(ctx, u, "❌ "+err.Error()+"\n\nपुन्हा password टाका:")
			setUserbotLogin(adminID, ls) // same phase वर retry साठी ठेवा
			return dispatcher.EndGroups
		}
		finishUserbotLogin(ctx, u, adminID, sessionStr, firstName, lastName, username, phoneOut, userID)
		return dispatcher.EndGroups
	}
	clearUserbotLogin(adminID)
	return dispatcher.EndGroups
}

func finishUserbotLogin(ctx *ext.Context, u *ext.Update, senderID int64, sessionStr, firstName, lastName, username, phone string, userID int64) {
	clearUserbotLogin(senderID)
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)
	var err error
	if isAdmin {
		err = db.SaveUserBotSession(sessionStr, phone, firstName, lastName, username, userID)
	} else {
		err = db.SaveUserBotSessionForUser(sessionStr, phone, firstName, lastName, username, userID, senderID)
	}
	if err != nil {
		reply(ctx, u, "⚠️ Login झालं पण session save error: "+err.Error())
		return
	}
	if isAdmin && UserbotStartFromSession != nil {
		UserbotStartFromSession(sessionStr)
	}
	name := strings.TrimSpace(firstName + " " + lastName)
	uname := "—"
	if username != "" {
		uname = "@" + username
	}
	reply(ctx, u, fmt.Sprintf("✅ Userbot Account Login यशस्वी झाला!\n\n👤 %s\n%s\n📱 %s\n🆔 UserID: %d\n\nआता तुम्ही Premium Tools आणि Userbot DL वापरू शकता.", name, uname, phone, userID))
}

// ═══════════════════════════════════════════════════════════════════════════
// Registry population
// ═══════════════════════════════════════════════════════════════════════════

func init() {
	// ── Users ──
	registerCmd(menuCmd{key: "listusers", label: "👤 List Users", kind: kindView, fn: cmdListUsers})
	registerCmd(menuCmd{key: "adduser", label: "➕ Add User", kind: kindFlow, fn: cmdAddUser, steps: []cmdStep{
		inputStep("📱 Mobile number टाका (नसेल तर - पाठवा):"),
		inputStep("📧 Email टाका (नसेल तर - पाठवा):"),
		inputStep("🔑 Password टाका:"),
		inputStep("👤 Full Name टाका (नसेल तर - पाठवा):"),
	}})
	registerCmd(menuCmd{key: "deluser", label: "🗑 Delete User", kind: kindFlow, fn: cmdDelUser, steps: []cmdStep{
		selectStep("🗑 Delete User — निवडा:", listUsersItems),
	}})

	// ── Course ──
	registerCmd(menuCmd{key: "listcourses", label: "📚 List Courses", kind: kindView, fn: cmdListCourses})
	registerCmd(menuCmd{key: "addcourse", label: "➕ Add Course", kind: kindFlow, fn: cmdAddCourse, steps: []cmdStep{
		inputStep("📚 एका ओळीत टाका: title | description | price | buy_link | channel_id\n\nExample:\nPython Basics | Learn Python from scratch | ₹999 | https://pay.example.com/python | -1001234567890"),
	}})
	registerCmd(menuCmd{key: "delcourse", label: "🗑 Delete Course", kind: kindFlow, fn: cmdDelCourse, steps: []cmdStep{
		selectStep("🗑 Delete Course — निवडा:", listCoursesItems),
	}})
	registerCmd(menuCmd{key: "setdlimit", label: "⚙️ Set Download Limit", kind: kindFlow, fn: cmdSetDLimit, steps: []cmdStep{
		selectStep("⚙️ कोणत्या Course चा limit बदलायचा:", listCoursesItems),
		inputStep("🔢 नवीन daily limit टाका (0 = default 3):"),
	}})
	registerCmd(menuCmd{key: "grantaccess", label: "🔑 Grant Access", kind: kindFlow, fn: cmdGrantAccess, steps: []cmdStep{
		selectStep("👤 कोणत्या User ला access द्यायचा:", listUsersItems),
		selectStep("📚 कोणता Course द्यायचा:", listCoursesItems),
		inputStep("📅 Access कालावधी टाका:\n  30d → 30 दिवस\n  3m  → 3 महिने\n  -   → permanent"),
	}})
	registerCmd(menuCmd{key: "revokeaccess", label: "🚫 Revoke Access", kind: kindFlow, fn: cmdRevokeAccess, steps: []cmdStep{
		selectStep("🚫 कोणता access काढायचा:", listAccessItems),
	}})
	registerCmd(menuCmd{key: "addvideo", label: "🎬 Add Video", kind: kindFlow, fn: cmdAddVideo, steps: []cmdStep{
		selectStep("🎬 कोणत्या Course मध्ये video add करायचा:", listCoursesItems),
		inputStep("🔢 Start Message ID टाका:"),
		inputStep("🔢 End Message ID टाका:"),
		inputStep("📝 Caption टाका:"),
	}})
	registerCmd(menuCmd{key: "syncchannel", label: "🔄 Sync Channel", kind: kindFlow, fn: cmdSyncChannel, steps: []cmdStep{
		selectStep("🔄 कोणता Course sync करायचा:", listCoursesItems),
		inputStep("🔢 Start Message ID टाका:"),
		inputStep("🔢 End Message ID टाका:"),
	}})
	registerCmd(menuCmd{key: "regstatus", label: "🚦 Registration Status", kind: kindView, fn: cmdRegStatus})
	registerCmd(menuCmd{key: "regopen", label: "🟢 Registration ON", kind: kindToggle, fn: cmdRegOpen})
	registerCmd(menuCmd{key: "regclose", label: "🔴 Registration OFF", kind: kindToggle, fn: cmdRegClose})
	registerCmd(menuCmd{key: "offlinewmstatus", label: "💧 Offline Watermark Status", kind: kindView, fn: cmdOfflineWmStatus})
	registerCmd(menuCmd{key: "offlinewmon", label: "🟢 Offline Watermark ON", kind: kindToggle, fn: cmdOfflineWmOn})
	registerCmd(menuCmd{key: "offlinewmoff", label: "🔴 Offline Watermark OFF", kind: kindToggle, fn: cmdOfflineWmOff})
	registerCmd(menuCmd{key: "setpaymentmode", label: "💰 Set Payment Mode", kind: kindFlow, fn: cmdSetPaymentMode, steps: []cmdStep{
		selectStep("💰 कोणत्या Course चा payment mode सेट करायचा:", listCoursesItems),
		selectStep("💰 कोणता mode हवा:", paymentModeItems),
	}})

	// ── Userbot ──
	registerCmd(menuCmd{key: "userbotstatus", label: "ℹ️ Userbot Status", kind: kindView, fn: cmdUserbotStatusView})
	registerCmd(menuCmd{key: "userbotlogin", label: "🔑 Userbot Login", kind: kindView, fn: cmdUserbotLoginStart})
	registerCmd(menuCmd{key: "userbotlogout", label: "🚪 Userbot Logout", kind: kindToggle, fn: cmdUserbotLogout})
	registerCmd(menuCmd{key: "adminstream", label: "📥 Admin Stream", kind: kindFlow, fn: cmdAdminStream, steps: []cmdStep{
		inputStep("🔢 LOG_CHANNEL चा Message ID टाका:"),
	}})
	registerCmd(menuCmd{key: "userbotdl", label: "⬇️ Userbot Download", kind: kindFlow, fn: cmdUserbotDL, steps: []cmdStep{
		inputStep("📥 ChatID आणि MsgID/Range टाका\n(उदा: -100123456789 45  किंवा  -100123456789 45 60):"),
	}})
	registerCmd(menuCmd{key: "stopuserbotdl", label: "🛑 Stop Userbot DL", kind: kindToggle, fn: cmdStopUserbotDL})

	// ── Sessions/Devices ──
	registerCmd(menuCmd{key: "sessions", label: "📱 Active Sessions", kind: kindView, fn: cmdSessions})
	registerCmd(menuCmd{key: "kill", label: "⚡ Kill Device", kind: kindFlow, fn: cmdKillDevice, steps: []cmdStep{
		selectStep("⚡ कोणत्या User चा device kill करायचा:", listUsersItems),
	}})
	registerCmd(menuCmd{key: "killsession", label: "⚡ Kill Session", kind: kindFlow, fn: cmdKillSession, steps: []cmdStep{
		selectStep("⚡ कोणतं session kill करायचं:", listActiveSessionsItems),
	}})

	// ── Stream Settings ──
	registerCmd(menuCmd{key: "listreplace", label: "📋 List Replace Rules", kind: kindView, fn: cmdListReplace})
	registerCmd(menuCmd{key: "setreplace", label: "➕ Set Replace Rule", kind: kindFlow, fn: cmdSetReplace, steps: []cmdStep{
		inputStep("📝 old_text=new_text टाका (line मध्ये space असली तरी चालेल):"),
	}})
	registerCmd(menuCmd{key: "delreplace", label: "🗑 Delete Replace Rule", kind: kindFlow, fn: cmdDelReplace, steps: []cmdStep{
		selectStep("🗑 Delete Replace Rule — निवडा:", listReplaceItems),
	}})
	registerCmd(menuCmd{key: "clearreplace", label: "🧹 Clear All Replace Rules", kind: kindToggle, fn: cmdClearReplace})
	registerCmd(menuCmd{key: "showsendchannel", label: "📢 Show Send Channel", kind: kindView, fn: cmdShowSendChannel})
	registerCmd(menuCmd{key: "setsendchannel", label: "➕ Set Send Channel", kind: kindFlow, fn: cmdSetSendChannel, steps: []cmdStep{
		inputStep("📢 Channel ID टाका:"),
	}})
	registerCmd(menuCmd{key: "clearsendchannel", label: "🗑 Clear Send Channel", kind: kindToggle, fn: cmdClearSendChannel})
	registerCmd(menuCmd{key: "setuserbotlog", label: "📁 Set Userbot Log Group", kind: kindFlow, fn: cmdSetUserbotLogGroup, steps: []cmdStep{
		inputStep("📢 Group ID टाका (t.me/c/ वरील ID):"),
	}})
	registerCmd(menuCmd{key: "clearuserbotlog", label: "🗑 Clear Userbot Log", kind: kindToggle, fn: cmdClearUserbotLogGroup})
	registerCmd(menuCmd{key: "showthumbnail", label: "📸 Show Thumbnail", kind: kindView, fn: cmdShowThumbnail})
	registerCmd(menuCmd{key: "setthumbnail", label: "➕ Set Thumbnail (Info)", kind: kindView, fn: cmdShowSetThumbnailHelp})
	registerCmd(menuCmd{key: "clearthumbnail", label: "🗑 Clear Thumbnail", kind: kindToggle, fn: cmdClearThumbnail})
	registerCmd(menuCmd{key: "showwatermark", label: "💧 Show Watermark", kind: kindView, fn: cmdShowWatermark})
	registerCmd(menuCmd{key: "setwatermark", label: "➕ Set Watermark", kind: kindFlow, fn: cmdSetWatermark, steps: []cmdStep{
		inputStep("💧 Watermark text टाका:"),
	}})
	registerCmd(menuCmd{key: "clearwatermark", label: "🗑 Clear Watermark", kind: kindToggle, fn: cmdClearWatermark})

	// ── Colab ──
	registerCmd(menuCmd{key: "colabstatus", label: "☁️ Colab Status", kind: kindView, fn: cmdColabStatus})
	registerCmd(menuCmd{key: "colabstart", label: "▶️ Colab Start", kind: kindToggle, fn: cmdColabStart})
	registerCmd(menuCmd{key: "colabstop", label: "⏸ Colab Stop", kind: kindToggle, fn: cmdColabStop})

	// ── Guard/Security ──
	registerCmd(menuCmd{key: "suspicious", label: "🚨 Suspicious Users", kind: kindView, fn: cmdSuspicious})
	registerCmd(menuCmd{key: "userinfo", label: "ℹ️ User Info", kind: kindFlow, fn: cmdUserInfo, steps: []cmdStep{
		selectStep("ℹ️ कोणत्या User ची माहिती हवी:", listUsersItems),
	}})
	registerCmd(menuCmd{key: "blockuser", label: "⛔ Block User", kind: kindFlow, fn: cmdBlockUser, steps: []cmdStep{
		inputStep("👤 Mobile/email टाका:"),
		inputStep("📅 किती days block करायचे (number):"),
		inputStep("📝 Reason टाका (नसेल तर - पाठवा):"),
	}})
	registerCmd(menuCmd{key: "unblockuser", label: "✅ Unblock User", kind: kindFlow, fn: cmdUnblockUser, steps: []cmdStep{
		selectStep("✅ कोणाला unblock करायचं:", listBlockedUsersItems),
	}})

	// ── Linking ──
	registerCmd(menuCmd{key: "settg", label: "🔗 Link Telegram ID", kind: kindFlow, fn: cmdSetTG, steps: []cmdStep{
		inputStep("👤 Mobile/email टाका:"),
		inputStep("🆔 Telegram ID टाका:"),
	}})
	registerCmd(menuCmd{key: "unlinktg", label: "🔓 Unlink Telegram (emergency)", kind: kindFlow, fn: cmdUnlinkTG, steps: []cmdStep{
		inputStep("👤 Mobile/email टाका:"),
	}})

	// ── Grant Userbot Access ──
	registerCmd(menuCmd{key: "grantuserbot", label: "🔑 Grant Userbot Access", kind: kindFlow, fn: cmdGrantUserbotAccess, steps: []cmdStep{
		inputStep("🔑 Telegram ID किंवा Mobile / Username टाका (उदा: 1234567890):"),
	}})

	// ── Pre-Download & Copy Channel ──
	registerCmd(menuCmd{key: "predownload", label: "⏳ Pre-Download Course", kind: kindFlow, fn: cmdPreDownload, steps: []cmdStep{
		selectStep("👤 Target User निवडा:", listUsersItems),
		selectStep("📚 Course निवडा:", listCoursesItems),
	}})
	registerCmd(menuCmd{key: "copychannel", label: "📤 Copy Channel", kind: kindFlow, fn: cmdCopyChannel, steps: []cmdStep{
		inputStep("📤 SourceChatID, DestChatID आणि Range/MsgID टाका\n(उदा: 2479194095 2588123456 1 500):"),
	}})

	// ── Stats ──
	registerCmd(menuCmd{key: "stats", label: "📊 Bot Stats", kind: kindView, fn: cmdStats})
}

// ═══════════════════════════════════════════════════════════════════════════
// Keyboard builders
// ═══════════════════════════════════════════════════════════════════════════

func buildRootMarkup() tg.ReplyMarkupClass {
	var rows []tg.KeyboardButtonRow
	var currentRow []tg.KeyboardButtonClass
	for _, c := range categories {
		btn := &tg.KeyboardButtonCallback{Text: c.label, Data: []byte("menu:cat:" + c.key)}
		currentRow = append(currentRow, btn)
		if len(currentRow) == 2 {
			rows = append(rows, tg.KeyboardButtonRow{Buttons: currentRow})
			currentRow = nil
		}
	}
	if len(currentRow) > 0 {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: currentRow})
	}
	return &tg.ReplyInlineMarkup{Rows: rows}
}

func buildCategoryMarkup(catKey string) tg.ReplyMarkupClass {
	var rows []tg.KeyboardButtonRow
	if cat := findCategory(catKey); cat != nil {
		var currentRow []tg.KeyboardButtonClass
		for _, key := range cat.cmdKeys {
			mc, ok := menuRegistry[key]
			if !ok {
				continue
			}
			btn := &tg.KeyboardButtonCallback{Text: mc.label, Data: []byte("cmd:" + key)}
			currentRow = append(currentRow, btn)
			if len(currentRow) == 2 {
				rows = append(rows, tg.KeyboardButtonRow{Buttons: currentRow})
				currentRow = nil
			}
		}
		if len(currentRow) > 0 {
			rows = append(rows, tg.KeyboardButtonRow{Buttons: currentRow})
		}
	}
	rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "⬅️ Back to Main Menu", Data: []byte("menu:root")},
	}})
	return &tg.ReplyInlineMarkup{Rows: rows}
}

func backAndRefreshMarkup(key string) tg.ReplyMarkupClass {
	backData := "menu:root"
	if ck := catKeyOf(key); ck != "" {
		backData = "menu:cat:" + ck
	}
	var rows []tg.KeyboardButtonRow
	if key != "" {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🔄 Refresh", Data: []byte("cmd:" + key)},
		}})
	}
	rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "⬅️ Back", Data: []byte(backData)},
	}})
	return &tg.ReplyInlineMarkup{Rows: rows}
}

func cancelMarkup() tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "❌ Cancel", Data: []byte("cancel")},
		}},
	}}
}

// ═══════════════════════════════════════════════════════════════════════════
// Callback-response plumbing (used by patched reply() in admin.go)
// ═══════════════════════════════════════════════════════════════════════════

func editMenu(ctx *ext.Context, u *ext.Update, text string, markup tg.ReplyMarkupClass) {
	cbq := u.CallbackQuery
	if cbq == nil {
		return
	}
	chatID := u.EffectiveUser().GetID()
	_, err := ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
		ID:          cbq.MsgID,
		Message:     text,
		ReplyMarkup: markup,
	})
	if err != nil && isMessageNotModifiedErr(err) {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: cbq.QueryID})
		return
	}
	_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: cbq.QueryID})
}

// answerCallbackText — reply()(admin.go) callback update असेल तर याला call करतो.
func answerCallbackText(ctx *ext.Context, u *ext.Update, text string) {
	key := ""
	if data := string(u.CallbackQuery.Data); strings.HasPrefix(data, "cmd:") {
		key = strings.TrimPrefix(data, "cmd:")
	}
	editMenu(ctx, u, text, backAndRefreshMarkup(key))
}

// ═══════════════════════════════════════════════════════════════════════════
// /cmd root
// ═══════════════════════════════════════════════════════════════════════════

func cmdMenuRoot(ctx *ext.Context, u *ext.Update) error {
	_, err := ctx.Reply(u, ext.ReplyTextString("📋 Admin Menu\n\nCategory निवडा:"), &ext.ReplyOpts{
		Markup: buildRootMarkup(),
	})
	if err != nil {
		utils.Logger.Sugar().Error(err)
	}
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Step rendering — एक callback-context (button दाबताच) आणि एक message-context
// (flow च्या पुढच्या step ला, plain reply नंतर)
// ═══════════════════════════════════════════════════════════════════════════

func renderSelectListText(step cmdStep, idxMap map[int]string) (string, error, string) {
	items, emptyMsg, err := step.listFn()
	if err != nil {
		return "", err, ""
	}
	if len(items) == 0 {
		return "", nil, emptyMsg
	}
	var sb strings.Builder
	sb.WriteString(step.prompt + "\n\n")
	for i, it := range items {
		idxMap[i+1] = it.id
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, it.display))
	}
	sb.WriteString("\n✏️ खालील number reply करून पाठवा (उदा. 1)")
	return sb.String(), nil, ""
}

// startFlowStep — button दाबताच (callback context) पहिला/पुढचा step दाखवतो.
func startFlowStep(ctx *ext.Context, u *ext.Update, key string, step cmdStep, remaining []cmdStep, adminID int64, collected []string) {
	switch step.kind {
	case stepSelect:
		idxMap := map[int]string{}
		text, err, emptyMsg := renderSelectListText(step, idxMap)
		if err != nil {
			editMenu(ctx, u, "❌ "+err.Error(), backAndRefreshMarkup(key))
			return
		}
		if emptyMsg != "" {
			editMenu(ctx, u, emptyMsg, backAndRefreshMarkup(key))
			return
		}
		setPending(adminID, &pendingAction{key: key, currentStep: step, remaining: remaining, indexMap: idxMap, collected: collected})
		editMenu(ctx, u, text, cancelMarkup())
	case stepInput:
		setPending(adminID, &pendingAction{key: key, currentStep: step, remaining: remaining, collected: collected})
		editMenu(ctx, u, step.prompt, cancelMarkup())
	}
}

// renderStepAsMessage — flow मध्ये पुढचा step असतो तेव्हा (plain message context,
// admin च्या मागच्या reply नंतर) प्रश्न/यादी पाठवतो.
func renderStepAsMessage(ctx *ext.Context, u *ext.Update, key string, step cmdStep, remaining []cmdStep, adminID int64, collected []string) {
	switch step.kind {
	case stepSelect:
		idxMap := map[int]string{}
		text, err, emptyMsg := renderSelectListText(step, idxMap)
		if err != nil {
			reply(ctx, u, "❌ "+err.Error())
			clearPending(adminID)
			return
		}
		if emptyMsg != "" {
			reply(ctx, u, emptyMsg)
			clearPending(adminID)
			return
		}
		setPending(adminID, &pendingAction{key: key, currentStep: step, remaining: remaining, indexMap: idxMap, collected: collected})
		reply(ctx, u, text)
	case stepInput:
		setPending(adminID, &pendingAction{key: key, currentStep: step, remaining: remaining, collected: collected})
		reply(ctx, u, step.prompt)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Callback handlers
// ═══════════════════════════════════════════════════════════════════════════

func handleMenuNav(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	data := string(u.CallbackQuery.Data)
	if data == "menu:root" {
		if isAdmin {
			editMenu(ctx, u, "📋 Admin Menu\n\nCategory निवडा:", buildRootMarkup())
		} else {
			text, markup, err := renderCourseListView(senderID)
			if err != nil {
				answerCatalogError(ctx, u, err)
				return dispatcher.EndGroups
			}
			editCatalogMessage(ctx, u, text, markup)
		}
		return dispatcher.EndGroups
	}
	catKey := strings.TrimPrefix(data, "menu:cat:")
	cat := findCategory(catKey)
	if cat == nil {
		if isAdmin {
			editMenu(ctx, u, "❌ Category सापडली नाही.", buildRootMarkup())
		} else {
			text, markup := renderPremiumMenuView(senderID)
			editCatalogMessage(ctx, u, text, markup)
		}
		return dispatcher.EndGroups
	}
	editMenu(ctx, u, cat.label+"\n\nCommand निवडा:", buildCategoryMarkup(cat.key))
	return dispatcher.EndGroups
}

func handleMenuCmd(ctx *ext.Context, u *ext.Update) error {
	key := strings.TrimPrefix(string(u.CallbackQuery.Data), "cmd:")
	mc, ok := menuRegistry[key]
	if !ok {
		editMenu(ctx, u, "❌ Command सापडला नाही.", buildRootMarkup())
		return dispatcher.EndGroups
	}
	senderID := u.EffectiveUser().GetID()
	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	if !isAdmin {
		if !db.HasUserbotAccess(senderID) {
			editCatalogMessage(ctx, u, "⛔ तुम्हाला या कमांडचा ॲक्सेस नाही.", &tg.ReplyInlineMarkup{Rows: backOnlyRow("wc:root")})
			return dispatcher.EndGroups
		}
	}

	switch mc.kind {
	case kindView, kindToggle:
		_ = mc.fn(ctx, u)

	case kindFlow:
		if len(mc.steps) == 0 {
			editMenu(ctx, u, "❌ Configuration error — steps नाहीत.", backAndRefreshMarkup(key))
			return dispatcher.EndGroups
		}
		startFlowStep(ctx, u, key, mc.steps[0], mc.steps[1:], senderID, nil)
	}
	return dispatcher.EndGroups
}

func handleMenuCancel(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	clearPending(senderID)

	isAdmin := utils.Contains(config.ValueOf.AllowedUsers, senderID)
	if isAdmin {
		editMenu(ctx, u, "❌ Cancel केलं.", buildRootMarkup())
	} else {
		text, markup := renderPremiumMenuView(senderID)
		editCatalogMessage(ctx, u, "❌ Cancel केलं.\n\n"+text, markup)
	}
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Pending plain-text reply catcher (select index / input answers)
// ═══════════════════════════════════════════════════════════════════════════

func handlePendingReply(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	if msg == nil || strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return dispatcher.ContinueGroups
	}
	senderID := u.EffectiveUser().GetID()
	// Check if user has pending action or login flow (allowed for all active users)
	if !utils.Contains(config.ValueOf.AllowedUsers, senderID) {
		if getPending(senderID) == nil && getUserbotLogin(senderID) == nil {
			return dispatcher.ContinueGroups
		}
	}

	// Userbot login flow (phone → OTP → 2FA) — generic pending फ्लो च्याही आधी check.
	if ls := getUserbotLogin(senderID); ls != nil {
		return handleUserbotLoginReply(ctx, u, senderID, ls, strings.TrimSpace(msg.Text))
	}

	p := getPending(senderID)
	if p == nil {
		return dispatcher.ContinueGroups
	}

	if p.key == "setsendchannel_user" {
		text := strings.TrimSpace(msg.Text)
		chID, err := strconv.ParseInt(text, 10, 64)
		if err != nil || chID == 0 {
			reply(ctx, u, "❌ Invalid Channel ID! पुन्हा प्रयत्न करा (उदा: `-1001234567890`):")
			return dispatcher.EndGroups
		}
		clearPending(senderID)
		setSendChannelIDForUser(senderID, chID)
		reply(ctx, u, fmt.Sprintf("✅ Destination Send Channel set झाला: `%d`", chID))
		return dispatcher.EndGroups
	}

	if p.key == "setwatermark_user" {
		text := strings.TrimSpace(msg.Text)
		clearPending(senderID)
		setWatermarkTextForUser(senderID, text)
		reply(ctx, u, fmt.Sprintf("✅ Custom Watermark set झाला: `%s`", text))
		return dispatcher.EndGroups
	}

	mc, ok := menuRegistry[p.key]
	if !ok {
		clearPending(senderID)
		return dispatcher.ContinueGroups
	}
	text := strings.TrimSpace(msg.Text)

	var value string
	switch p.currentStep.kind {
	case stepSelect:
		idx, err := strconv.Atoi(text)
		if err != nil {
			reply(ctx, u, "❌ फक्त number पाठवा (उदा. 1). पुन्हा प्रयत्न करा:")
			return dispatcher.EndGroups
		}
		id, ok2 := p.indexMap[idx]
		if !ok2 {
			reply(ctx, u, "❌ चुकीचा number. यादीतील बरोबर number पाठवा:")
			return dispatcher.EndGroups
		}
		value = id
	case stepInput:
		value = text
	}

	collected := append(p.collected, value)

	if len(p.remaining) > 0 {
		next := p.remaining[0]
		rest := p.remaining[1:]
		clearPending(senderID)
		renderStepAsMessage(ctx, u, p.key, next, rest, senderID, collected)
		return dispatcher.EndGroups
	}

	clearPending(senderID)
	msg.Text = "/" + p.key + " " + strings.Join(collected, " ")
	_ = mc.fn(ctx, u)
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════
// Load — commands.go चा reflect-based Load() हे auto call करतो
// ═══════════════════════════════════════════════════════════════════════════

func (m *command) LoadMenu(d dispatcher.Dispatcher) {
	log := m.log.Named("menu")
	defer log.Sugar().Info("Loaded button menu")

	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("menu:"), handleMenuNav))
	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("cmd:"), handleMenuCmd))
	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Equal("cancel"), handleMenuCancel))

	// Group -2 — relay(-1) आणि commands(0) च्याही आधी चालतो, जेणेकरून pending
	// असलेल्या admin चा plain-text reply आधी इथे check होईल.
	d.AddHandlerToGroup(handlers.NewMessage(nil, handlePendingReply), -2)
}







func premiumCancelMarkup() tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "⬅️ Back to Userbot Menu", Data: []byte("wc:premium_menu")},
		}},
	}}
}

func setPendingUserbotDLPrompt(ctx *ext.Context, u *ext.Update, userID int64) {
	setPending(userID, &pendingAction{
		key: "userbotdl",
		currentStep: inputStep("📥 *Userbot Download*\n\nChat ID आणि Message Range/ID टाका किंवा Post Link वापरा:\n\n*Examples:*\n`-100123456789 45` (Single message)\n`-100123456789 45 60` (Range of messages)\n`https://t.me/c/4372880940/1687` (Post Link)"),
	})
	editCatalogMessage(ctx, u, "📥 *Userbot Download*\n\nChat ID आणि Message Range/ID टाका किंवा Post Link वापरा:\n\n*Examples:*\n`-100123456789 45` (Single message)\n`-100123456789 45 60` (Range of messages)\n`https://t.me/c/4372880940/1687` (Post Link)", premiumCancelMarkup())
}

func setPendingSendChannelPrompt(ctx *ext.Context, u *ext.Update, userID int64) {
	setPending(userID, &pendingAction{
		key: "setsendchannel_user",
		currentStep: inputStep("📢 *Set Destination Channel*\n\nDownloads पाठवण्यासाठी तुमचा Channel ID टाका (उदा: `-1001234567890`):"),
	})
	editCatalogMessage(ctx, u, "📢 *Set Destination Channel*\n\nDownloads पाठवण्यासाठी तुमचा Channel ID टाका (उदा: `-1001234567890`):", premiumCancelMarkup())
}

func setPendingWatermarkPrompt(ctx *ext.Context, u *ext.Update, userID int64) {
	setPending(userID, &pendingAction{
		key: "setwatermark_user",
		currentStep: inputStep("💧 *Set Watermark Text*\n\nVideos वर दाखवायचा तुमचा Watermark text टाका (उदा: `@mychannel`):"),
	})
	editCatalogMessage(ctx, u, "💧 *Set Watermark Text*\n\nVideos वर दाखवायचा तुमचा Watermark text टाका (उदा: `@mychannel`):", premiumCancelMarkup())
}
