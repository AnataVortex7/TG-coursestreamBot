// internal/commands/autosync_ui.go
//
// AutoSync — admin bot controls.
//
//   /autosync            tasks chi yadi + status
//   /asadd               navin task wizard (account → source → destination → mode → times → replace → start)
//   /asaccount           navin userbot account add (phone → OTP → 2FA)
//   /asrun <id>          task lagech chalva
//   /astoggle <id>       pause / resume
//   /astimes <id> <t..>  daily times badla   (e.g. 09:00, 21:30 — IST)
//   /asreplace <id> <rules>   task che replace words (old=>new, ek ola = ek rule; "-" = kadhun taka)
//   /asreset <id> <msgid>     source cursor hala (0 = shevatpasun sagle parat)
//   /asdel <id> DELETE   task delete
//
// Sagle menu madhun pan (🔁 Auto Sync category) vaparta yetat.
package commands

import (
	"context"
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
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func primitiveObjectID(s string) (primitive.ObjectID, error) {
	return primitive.ObjectIDFromHex(strings.TrimSpace(s))
}

// ─── wizard state ────────────────────────────────────────────────────────────

const (
	asStepName = iota
	asStepAccount
	asStepSource
	asStepDest
	asStepTopics
	asStepMode
	asStepTimes
	asStepReplace
	asStepStart
	asStepConfirm
)

type asWizard struct {
	mu         sync.Mutex
	step       int
	name       string
	sessions   []db.UserBotSessionDoc
	sessionKey string
	sessionLbl string
	chats      []*asChat
	src, dst   *asChat
	mirror     bool
	mode       string
	times      []string
	replaces   []db.ReplaceRule
	startKind  int // 0 = fakt navin (ata pasun), 1 = suruvatipasun, 2 = specific id
	startID    int
	expires    time.Time
}

const asWizardTTL = 15 * time.Minute

var (
	asWizMu sync.Mutex
	asWiz   = map[int64]*asWizard{}
)

func getASWizard(id int64) *asWizard {
	asWizMu.Lock()
	defer asWizMu.Unlock()
	w, ok := asWiz[id]
	if !ok {
		return nil
	}
	if time.Now().After(w.expires) {
		delete(asWiz, id)
		return nil
	}
	return w
}

func setASWizard(id int64, w *asWizard) {
	w.expires = time.Now().Add(asWizardTTL)
	asWizMu.Lock()
	asWiz[id] = w
	asWizMu.Unlock()
}

func clearASWizard(id int64) {
	asWizMu.Lock()
	delete(asWiz, id)
	asWizMu.Unlock()
}

func touchASWizard(w *asWizard) {
	asWizMu.Lock()
	w.expires = time.Now().Add(asWizardTTL)
	asWizMu.Unlock()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func asSessionLabel(d db.UserBotSessionDoc) string {
	kind := "System"
	if strings.HasPrefix(d.Key, "user_") {
		kind = "Extra/User"
	}
	nm := strings.TrimSpace(d.FirstName + " " + d.LastName)
	if d.Username != "" {
		nm += " (@" + d.Username + ")"
	}
	if strings.TrimSpace(nm) == "" {
		nm = d.Phone
	}
	return fmt.Sprintf("%s — %s", nm, kind)
}

func asSessionLabelByKey(key string) string {
	docs, err := db.GetAllUserBotSessions()
	if err == nil {
		for _, d := range docs {
			if d.Key == key {
				return asSessionLabel(d)
			}
		}
	}
	return key
}

func asChatLine(i int, c *asChat) string {
	tag := "📢"
	if !c.Broadcast {
		tag = "👥"
	}
	if c.Forum {
		tag = "🗂"
	}
	return fmt.Sprintf("%d. %s %s (%d)", i, tag, clipRunes(c.Title, 40), c.ID)
}

// asCleanLine — control characters (CR/LF/NUL …) kadhto.
func asCleanLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

// asBaseCtx — background jobs sathi (app band hoeparyant).
func asBaseCtx() context.Context { return context.Background() }

// asButtonsMarkup — AutoSync quick buttons (Userbot Login sahit). "cmd:<key>" = menu registry commands.
func asButtonsMarkup() tg.ReplyMarkupClass {
	btn := func(text, data string) tg.KeyboardButtonClass {
		return &tg.KeyboardButtonCallback{Text: text, Data: []byte(data)}
	}
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{btn("🔑 Add Userbot Login", "cmd:asaccount")}},
		{Buttons: []tg.KeyboardButtonClass{btn("➕ New Task", "cmd:asadd"), btn("📋 Tasks", "cmd:autosync")}},
		{Buttons: []tg.KeyboardButtonClass{btn("⬅️ Menu", "menu:root")}},
	}}
}

// asReplyWithButtons — callback asel tar message edit, nahitar navin message; donhi var buttons.
func asReplyWithButtons(ctx *ext.Context, u *ext.Update, text string) {
	m := asButtonsMarkup()
	if u.CallbackQuery != nil {
		editMenu(ctx, u, text, m)
		return
	}
	_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: m})
}

func asWizardCancelHint() string { return "\n\n(Cancel: /cancel)" }

// ─── wizard handler ──────────────────────────────────────────────────────────

func handleAutoSyncWizard(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	if msg == nil || u.EffectiveUser() == nil {
		return dispatcher.ContinueGroups
	}
	senderID := u.EffectiveUser().GetID()
	w := getASWizard(senderID)
	if w == nil || !utils.Contains(config.ValueOf.AllowedUsers, senderID) {
		return dispatcher.ContinueGroups
	}
	text := strings.TrimSpace(msg.Text)
	low := strings.ToLower(text)
	if low == "/cancel" || low == "cancel" {
		clearASWizard(senderID)
		reply(ctx, u, "❌ AutoSync wizard cancel kela.")
		return dispatcher.EndGroups
	}
	if strings.HasPrefix(text, "/") {
		clearASWizard(senderID) // dusra command — wizard sodto
		return dispatcher.ContinueGroups
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Logger.Sugar().Errorw("autosync wizard panic", "panic", r)
				clearASWizard(senderID)
				reply(ctx, u, "❌ Anapekshit error — /asadd punha suru kara.")
			}
		}()
		w.mu.Lock()
		defer w.mu.Unlock()
		asWizardStep(ctx, u, senderID, w, text)
	}()
	return dispatcher.EndGroups
}

func asPromptSource(ctx *ext.Context, u *ext.Update, w *asWizard, which string) {
	var sb strings.Builder
	if which == "src" {
		sb.WriteString("📡 SOURCE chat select kara (jithun messages ghyaychet)\n\n")
	} else {
		sb.WriteString("📥 DESTINATION chat select kara (jithe messages takaychet)\n\n")
	}
	shown := w.chats
	if len(shown) > 40 {
		shown = shown[:40]
	}
	if len(shown) == 0 {
		sb.WriteString("(Account madhe channel/group sapadla nahi.)\n")
	}
	for i, c := range shown {
		sb.WriteString(asChatLine(i+1, c) + "\n")
	}
	sb.WriteString("\n✏️ Yadi madhla number pathva, kiva Chat ID type kara (-100... kiva t.me/c/ cha positive ID)")
	if which == "dst" {
		sb.WriteString("\n⚠️ Userbot ne destination madhe post karnyacha access (admin) asava.")
	}
	sb.WriteString(asWizardCancelHint())
	reply(ctx, u, sb.String())
}

// asPickChat — number (yadi) kiva Chat ID.
func asPickChat(cctx context.Context, w *asWizard, text string) (*asChat, error) {
	text = strings.TrimSpace(text)
	if n, err := strconv.Atoi(text); err == nil && len(text) <= 3 {
		shown := w.chats
		if len(shown) > 40 {
			shown = shown[:40]
		}
		if n >= 1 && n <= len(shown) {
			return shown[n-1], nil
		}
		return nil, fmt.Errorf("yadit %d number nahi", n)
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil || v == 0 {
		return nil, fmt.Errorf("number kiva valid Chat ID pathva")
	}
	if v > 0 {
		v = -1000000000000 - v
	}
	c, err := asClientForKey(cctx, w.sessionKey)
	if err != nil {
		return nil, err
	}
	return asResolveChat(cctx, c, v)
}

func asWizardStep(ctx *ext.Context, u *ext.Update, senderID int64, w *asWizard, text string) {
	touchASWizard(w)
	cctx, cancel := context.WithTimeout(asBaseCtx(), 90*time.Second)
	defer cancel()

	switch w.step {

	case asStepName:
		name := strings.TrimSpace(asCleanLine(text))
		if name == "" || len([]rune(name)) > 60 {
			reply(ctx, u, "❌ Task cha naav 1-60 characters dya:")
			return
		}
		docs, err := db.GetAllUserBotSessions()
		if err != nil || len(docs) == 0 {
			clearASWizard(senderID)
			asReplyWithButtons(ctx, u, "❌ Ekahi userbot account login nahi.\n\n🔑 Khali cha button ne account login kara, mag ➕ New Task.")
			return
		}
		w.name = name
		w.sessions = docs
		var sb strings.Builder
		sb.WriteString("🤖 Konta USERBOT ACCOUNT vaparaycha?\n\n")
		for i, d := range docs {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, asSessionLabel(d)))
		}
		sb.WriteString("\nNumber pathva. (Naya account hava tar khali 🔑 button — wizard cancel hoil, login nantar /asadd punha)" + asWizardCancelHint())
		w.step = asStepAccount
		asReplyWithButtons(ctx, u, sb.String())

	case asStepAccount:
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > len(w.sessions) {
			reply(ctx, u, "❌ Yadi madhla number pathva:")
			return
		}
		d := w.sessions[n-1]
		client, cerr := asClientForKey(cctx, d.Key)
		if cerr != nil {
			reply(ctx, u, "❌ Ha account chalu hot nahi: "+cerr.Error()+"\nDusra number pathva:")
			return
		}
		list, lerr := asListDialogs(cctx, client)
		if lerr != nil {
			reply(ctx, u, "❌ Chats load zale nahit: "+lerr.Error()+"\nPunha number pathva:")
			return
		}
		w.sessionKey = d.Key
		w.sessionLbl = asSessionLabel(d)
		w.chats = list
		w.step = asStepSource
		asPromptSource(ctx, u, w, "src")

	case asStepSource:
		c, err := asPickChat(cctx, w, text)
		if err != nil {
			reply(ctx, u, "❌ "+err.Error()+"\nPunha pathva:")
			return
		}
		w.src = c
		w.step = asStepDest
		asPromptSource(ctx, u, w, "dst")

	case asStepDest:
		c, err := asPickChat(cctx, w, text)
		if err != nil {
			reply(ctx, u, "❌ "+err.Error()+"\nPunha pathva:")
			return
		}
		if w.src != nil && c.ID == w.src.ID {
			reply(ctx, u, "❌ Source ani Destination same nako. Dusra pathva:")
			return
		}
		w.dst = c
		warn := ""
		if !c.CanPost {
			warn = "\n⚠️ Ha broadcast channel ahe — userbot admin nasel tar post fail hoil."
		}
		if w.src.Forum && c.Forum {
			w.step = asStepTopics
			reply(ctx, u, fmt.Sprintf("✅ Destination: %s%s\n\n🗂 Source madhe TOPICS ahet ani destination pan forum ahe.\nSame topics destination madhe aapoaap banvun, message tya topic madhe takaycha?\n\n1 = Ho (topics mirror kara)\n2 = Nahi (sagle ekach thikani)%s", c.Title, warn, asWizardCancelHint()))
			return
		}
		w.mirror = false
		extra := ""
		if w.src.Forum && !c.Forum {
			extra = "\nℹ️ Source forum ahe pan destination forum nahi — topics mirror honar nahit (sagle ekach thikani jatil)."
		}
		w.step = asStepMode
		reply(ctx, u, fmt.Sprintf("✅ Destination: %s%s%s\n\n%s", c.Title, warn, extra, asModePrompt()))

	case asStepTopics:
		switch strings.ToLower(text) {
		case "1", "ho", "yes", "y":
			w.mirror = true
		case "2", "nahi", "no", "n":
			w.mirror = false
		default:
			reply(ctx, u, "❌ 1 (ho) kiva 2 (nahi) pathva:")
			return
		}
		w.step = asStepMode
		reply(ctx, u, asModePrompt())

	case asStepMode:
		switch strings.ToLower(text) {
		case "1", "fast":
			w.mode = "fast"
		case "2", "full":
			w.mode = "full"
		default:
			reply(ctx, u, "❌ 1 (fast) kiva 2 (full) pathva:")
			return
		}
		w.step = asStepTimes
		reply(ctx, u, "🕒 Daily kiti vajta sync karaycha? (IST, ek kiva jast)\n\nExample:\n09:00, 14:30, 21:00\nkiva: 9am, 6pm"+asWizardCancelHint())

	case asStepTimes:
		times, err := asNormalizeTimes(text)
		if err != nil {
			reply(ctx, u, "❌ "+err.Error()+"\nPunha pathva:")
			return
		}
		w.times = times
		w.step = asStepReplace
		reply(ctx, u, "✏️ REPLACE WORDS (caption/text madhe badal) — ek ola = ek rule:\n\nold => new\nline: old => new   (ha shabd asleli purna ola kadhun taka)\n\nKahi nako tar - pathva.\nℹ️ Admin che global replace rules (/listreplace) pan lagtat."+asWizardCancelHint())

	case asStepReplace:
		w.replaces = nil
		if strings.TrimSpace(text) != "-" {
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				r, err := parseReplaceSpec(line)
				if err != nil {
					reply(ctx, u, fmt.Sprintf("❌ `%s` — %s\nSagle rules punha pathva (kiva - ):", clipRunes(line, 40), err.Error()))
					return
				}
				w.replaces = append(w.replaces, r)
			}
		}
		w.step = asStepStart
		reply(ctx, u, "▶️ Konapasun sync suru karaycha?\n\n0 = Fakt NAVIN messages (ata pasun pudhche)\n1 = Suruvatipasun sagle (junne pan)\nkiva ek Message ID pathva = to ani tyanantarche\n\n(Duplicate honar nahi.)"+asWizardCancelHint())

	case asStepStart:
		switch strings.ToLower(text) {
		case "0", "new", "navin":
			w.startKind = 0
		case "1", "all", "sagle":
			w.startKind = 1
		default:
			n, err := strconv.Atoi(text)
			if err != nil || n < 2 {
				reply(ctx, u, "❌ 0, 1 kiva Message ID (number) pathva:")
				return
			}
			w.startKind, w.startID = 2, n
		}
		w.step = asStepConfirm
		reply(ctx, u, asWizardSummary(w)+"\n\n✅ Task save karu? 1 = Ho, 2 = Cancel")

	case asStepConfirm:
		switch strings.ToLower(text) {
		case "1", "ho", "yes", "y":
		default:
			clearASWizard(senderID)
			reply(ctx, u, "❌ Cancel kela. (/asadd ne punha suru karta yeil)")
			return
		}
		client, err := asClientForKey(cctx, w.sessionKey)
		if err != nil {
			reply(ctx, u, "❌ Userbot chalu nahi: "+err.Error()+"\nPunha 1 pathva kiva 2 ne cancel kara:")
			return
		}
		cursor := 0
		switch w.startKind {
		case 0:
			cursor = asLatestID(cctx, client.API(), w.src.Peer)
		case 2:
			cursor = w.startID - 1
		}
		t := &db.AutoSyncTask{
			Name:         w.name,
			Enabled:      true,
			SessionKey:   w.sessionKey,
			OwnerID:      senderID,
			SourceChat:   w.src.ID,
			SourceTitle:  w.src.Title,
			DestChat:     w.dst.ID,
			DestTitle:    w.dst.Title,
			MirrorTopics: w.mirror,
			Mode:         w.mode,
			Times:        w.times,
			Replaces:     w.replaces,
			LastMsgID:    cursor,
			LastRunAt:    time.Now(), // creation chya veli missed slot catch-up nako
			LastStatus:   "Navin task — pahila run next scheduled time la",
		}
		if err := db.CreateAutoSyncTask(t); err != nil {
			reply(ctx, u, "❌ Save error: "+err.Error())
			return
		}
		clearASWizard(senderID)
		reply(ctx, u, fmt.Sprintf("🎉 AutoSync task tayar!\n\n“%s” → %s\n🆔 %s\n\nLagech test: /asrun %s\nSagle tasks: /autosync", t.Name, strings.Join(t.Times, ", ")+" IST", t.ID.Hex(), t.ID.Hex()))
	}
}

func asModePrompt() string {
	return "⚙️ MODE select kara:\n\n1 = ⚡ Fast — download nahi, reference ne copy (jalad). Fail zalyas aapoaap Full vaprto.\n2 = 🐢 Full — userbot download + re-upload (protected / no-forward channels sathi pan chalto, slow)." + asWizardCancelHint()
}

func asWizardSummary(w *asWizard) string {
	start := "Fakt navin messages (ata pasun)"
	switch w.startKind {
	case 1:
		start = "Suruvatipasun sagle"
	case 2:
		start = fmt.Sprintf("Message ID %d pasun", w.startID)
	}
	topics := "Nahi"
	if w.mirror {
		topics = "Ho (same topics banvle jatil)"
	}
	rep := "Nahi (fakt global rules)"
	if len(w.replaces) > 0 {
		rep = fmt.Sprintf("%d rule(s)", len(w.replaces))
	}
	return fmt.Sprintf("📋 SUMMARY\n\n📝 Naav: %s\n🤖 Account: %s\n📡 Source: %s (%d)\n📥 Destination: %s (%d)\n🗂 Topics mirror: %s\n⚙️ Mode: %s\n🕒 Times (IST): %s\n✏️ Replace: %s\n▶️ Start: %s",
		w.name, w.sessionLbl, w.src.Title, w.src.ID, w.dst.Title, w.dst.ID, topics, w.mode, strings.Join(w.times, ", "), rep, start)
}

// ─── commands ────────────────────────────────────────────────────────────────

func cmdASAdd(ctx *ext.Context, u *ext.Update) error {
	senderID := u.EffectiveUser().GetID()
	setASWizard(senderID, &asWizard{step: asStepName})
	reply(ctx, u, "🔁 NAVIN AUTOSYNC TASK\n\n📝 Task la naav dya (e.g. Python Course Sync):"+asWizardCancelHint())
	return dispatcher.EndGroups
}

func cmdASAccount(ctx *ext.Context, u *ext.Update) error {
	if UserbotSendCode == nil || UserbotVerifyCode == nil {
		reply(ctx, u, "❌ Userbot bridge ajun ready nahi (bot restart zala ka te bagha).")
		return dispatcher.EndGroups
	}
	senderID := u.EffectiveUser().GetID()
	clearASWizard(senderID) // login replies wizard ne khau nayet
	slot := int64(0)
	for n := int64(1); n <= 50; n++ {
		o := asSlotOwnerBase + n
		if d, _ := db.GetUserBotSessionDocForUser(o); d == nil || d.Session == "" {
			slot = o
			break
		}
	}
	if slot == 0 {
		reply(ctx, u, "❌ Max 50 extra accounts.")
		return dispatcher.EndGroups
	}
	setUserbotLogin(senderID, &loginState{phase: loginPhasePhone, slotOwner: slot})
	reply(ctx, u, "📱 NAVA USERBOT ACCOUNT ADD\n\nAccount cha mobile number tak (country code sobat, e.g. +919876543210):\n\nHa account tumcha system userbot badalnar nahi — fakt AutoSync tasks sathi.")
	return dispatcher.EndGroups
}

func asTaskFromArgs(ctx *ext.Context, u *ext.Update) (*db.AutoSyncTask, []string) {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "❌ Task ID dya. Yadi: /autosync")
		return nil, nil
	}
	oid, err := primitiveObjectID(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid task ID.")
		return nil, nil
	}
	t, err := db.GetAutoSyncTask(oid)
	if err != nil || t == nil {
		reply(ctx, u, "❌ Task sapadla nahi.")
		return nil, nil
	}
	return t, a[1:]
}

func cmdASRun(ctx *ext.Context, u *ext.Update) error {
	t, _ := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	id := t.ID.Hex()
	if _, running := asRunning.Load(id); running {
		reply(ctx, u, "⏳ Ha task adhich chalu ahe.")
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("▶️ “%s” suru kela… result thodya velane yeil (bot tumhala message pathavel).", t.Name))
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Logger.Sugar().Errorw("autosync manual run panic", "panic", r)
				asRunning.Delete(id)
			}
		}()
		runAutoSyncTask(asBaseCtx(), id, true)
	}()
	return dispatcher.EndGroups
}

func cmdASToggle(ctx *ext.Context, u *ext.Update) error {
	t, _ := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	nv := !t.Enabled
	if err := db.SetAutoSyncFields(t.ID, bson.M{"enabled": nv}); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	st := "⏸ PAUSED"
	if nv {
		st = "🟢 ENABLED"
	}
	reply(ctx, u, fmt.Sprintf("%s — “%s”", st, t.Name))
	return dispatcher.EndGroups
}

func cmdASDel(ctx *ext.Context, u *ext.Update) error {
	t, rest := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	if len(rest) < 1 || rest[0] != "DELETE" {
		reply(ctx, u, fmt.Sprintf("⚠️ Confirm karayla: /asdel %s DELETE", t.ID.Hex()))
		return dispatcher.EndGroups
	}
	if err := db.DeleteAutoSyncTask(t.ID); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("🗑 Task delete zala: “%s” (destination madhle messages tase rahatil).", t.Name))
	return dispatcher.EndGroups
}

func cmdASTimes(ctx *ext.Context, u *ext.Update) error {
	t, rest := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	times, err := asNormalizeTimes(strings.Join(rest, " "))
	if err != nil {
		reply(ctx, u, "❌ "+err.Error()+"\nExample: /astimes "+t.ID.Hex()+" 09:00, 21:30")
		return dispatcher.EndGroups
	}
	if err := db.SetAutoSyncFields(t.ID, bson.M{"times": times, "last_run_at": time.Now()}); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("🕒 “%s” — navin times (IST): %s", t.Name, strings.Join(times, ", ")))
	return dispatcher.EndGroups
}

func cmdASReplace(ctx *ext.Context, u *ext.Update) error {
	t, _ := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	rest := commandRest(u.EffectiveMessage.Text) // "<id>\nrule\nrule" kiva "<id> rule"
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), t.ID.Hex()))
	if rest == "" {
		cur := "(kahi nahi)"
		if len(t.Replaces) > 0 {
			var parts []string
			for _, r := range t.Replaces {
				parts = append(parts, ruleDisplay(r))
			}
			cur = strings.Join(parts, "\n")
		}
		reply(ctx, u, fmt.Sprintf("✏️ “%s” replace rules:\n%s\n\nBadalayla: /asreplace %s old => new (multi-line chalto; purani list badli jate)\nKadhayla: /asreplace %s -", t.Name, cur, t.ID.Hex(), t.ID.Hex()))
		return dispatcher.EndGroups
	}
	var rules []db.ReplaceRule
	if rest != "-" {
		for _, line := range strings.Split(rest, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			r, err := parseReplaceSpec(line)
			if err != nil {
				reply(ctx, u, fmt.Sprintf("❌ `%s` — %s", clipRunes(line, 40), err.Error()))
				return dispatcher.EndGroups
			}
			rules = append(rules, r)
		}
	}
	if err := db.SetAutoSyncFields(t.ID, bson.M{"replaces": rules}); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("✅ “%s” — %d replace rule(s) set.", t.Name, len(rules)))
	return dispatcher.EndGroups
}

func cmdASReset(ctx *ext.Context, u *ext.Update) error {
	t, rest := asTaskFromArgs(ctx, u)
	if t == nil {
		return dispatcher.EndGroups
	}
	if len(rest) < 1 {
		reply(ctx, u, fmt.Sprintf("Usage: /asreset %s <msgid>\n\n0 = suruvatipasun sagle, kiva ek source Message ID = to ani tyanantarche parat copy.\n⚠️ Destination madhe duplicate hou shaktat.", t.ID.Hex()))
		return dispatcher.EndGroups
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 0 {
		reply(ctx, u, "❌ Valid Message ID (number) dya.")
		return dispatcher.EndGroups
	}
	cursor := n - 1
	if cursor < 0 {
		cursor = 0
	}
	_ = db.AutoSyncMapDeleteFrom(t.ID, cursor)
	if err := db.SetAutoSyncFields(t.ID, bson.M{"last_msg_id": cursor}); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("🔄 “%s” cursor reset: msg %d pasun parat sync hoil (/asrun %s ne lagech).", t.Name, cursor+1, t.ID.Hex()))
	return dispatcher.EndGroups
}

func cmdASList(ctx *ext.Context, u *ext.Update) error {
	tasks, err := db.ListAutoSyncTasks()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	if len(tasks) == 0 {
		asReplyWithButtons(ctx, u, "🔁 AutoSync\n\nAjun ekahi task nahi.\n\nAadhi 🔑 userbot account login kara (nasel tar), mag ➕ New Task.")
		return dispatcher.EndGroups
	}
	loc := istLoc()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔁 AutoSync Tasks (%d)\n", len(tasks)))
	for i, t := range tasks {
		st := "🟢 ON"
		if !t.Enabled {
			st = "⏸ PAUSED"
		}
		if _, running := asRunning.Load(t.ID.Hex()); running {
			st = "⏳ RUNNING"
		}
		topics := "no"
		if t.MirrorTopics {
			topics = "yes"
		}
		last := "—"
		if !t.LastRunAt.IsZero() {
			last = t.LastRunAt.In(loc).Format("02 Jan 15:04")
		}
		block := fmt.Sprintf("\n%d. %s [%s]\n🆔 %s\n📡 %s → 📥 %s\n🤖 %s\n⚙️ %s | topics: %s | 🕒 %s IST\n📊 Copied: %d | Last: %s\n%s\n",
			i+1, t.Name, st, t.ID.Hex(),
			clipRunes(t.SourceTitle, 28), clipRunes(t.DestTitle, 28),
			asSessionLabelByKey(t.SessionKey),
			t.Mode, topics, strings.Join(t.Times, ", "),
			t.TotalCopied, last, clipRunes(t.LastStatus, 160))
		if sb.Len()+len(block) > 3600 {
			sb.WriteString("\n…(baki tasks: IDs sathi pudhe select-list vapra)")
			break
		}
		sb.WriteString(block)
	}
	sb.WriteString("\n/asrun /astoggle /astimes /asreplace /asreset /asdel")
	asReplyWithButtons(ctx, u, sb.String())
	return dispatcher.EndGroups
}

func listASTaskItems() ([]listItem, string, error) {
	tasks, err := db.ListAutoSyncTasks()
	if err != nil {
		return nil, "", err
	}
	var items []listItem
	for _, t := range tasks {
		st := "🟢"
		if !t.Enabled {
			st = "⏸"
		}
		items = append(items, listItem{
			display: fmt.Sprintf("%s %s (%s → %s)", st, t.Name, clipRunes(t.SourceTitle, 18), clipRunes(t.DestTitle, 18)),
			id:      t.ID.Hex(),
		})
	}
	return items, "🔁 Ajun ekahi AutoSync task nahi. Pahila banva: ➕ New Sync Task", nil
}

// ─── registration ────────────────────────────────────────────────────────────

func init() {
	registerCmd(menuCmd{key: "autosync", label: "📋 Sync Tasks (List)", kind: kindView, fn: adminOnly(cmdASList)})
	registerCmd(menuCmd{key: "asadd", label: "➕ New Sync Task", kind: kindView, fn: adminOnly(cmdASAdd)})
	registerCmd(menuCmd{key: "asaccount", label: "🤖 Add Userbot Account", kind: kindView, fn: adminOnly(cmdASAccount)})
	registerCmd(menuCmd{key: "asrun", label: "▶️ Run Task Now", kind: kindFlow, fn: adminOnly(cmdASRun), steps: []cmdStep{
		selectStep("▶️ Konta task lagech chalvaycha?", listASTaskItems),
	}})
	registerCmd(menuCmd{key: "astoggle", label: "⏯ Pause / Resume", kind: kindFlow, fn: adminOnly(cmdASToggle), steps: []cmdStep{
		selectStep("⏯ Konta task pause/resume?", listASTaskItems),
	}})
	registerCmd(menuCmd{key: "astimes", label: "🕒 Change Times", kind: kindFlow, fn: adminOnly(cmdASTimes), steps: []cmdStep{
		selectStep("🕒 Konatya task che times badlaiche?", listASTaskItems),
		inputStep("🕒 Navin daily times (IST) pathva — e.g. 09:00, 21:30"),
	}})
	registerCmd(menuCmd{key: "asreplace", label: "✏️ Task Replace Words", kind: kindFlow, fn: adminOnly(cmdASReplace), steps: []cmdStep{
		selectStep("✏️ Konatya task che replace words?", listASTaskItems),
		inputStep("✏️ Ek rule pathva: old => new   (kiva - = sagle kadhun taka)\nMulti-line sathi command vapra: /asreplace <id> + rules"),
	}})
	registerCmd(menuCmd{key: "asreset", label: "🔄 Reset Cursor", kind: kindFlow, fn: adminOnly(cmdASReset), steps: []cmdStep{
		selectStep("🔄 Konatya task cha cursor reset?", listASTaskItems),
		inputStep("🔄 Source Message ID pathva (0 = suruvatipasun sagle)\n⚠️ Destination madhe duplicate hou shaktat."),
	}})
	registerCmd(menuCmd{key: "asdel", label: "🗑 Delete Task", kind: kindFlow, fn: adminOnly(cmdASDel), steps: []cmdStep{
		selectStep("🗑 Konta task delete?", listASTaskItems),
		inputStep("⚠️ Confirm karayla DELETE lihā:"),
	}})

	categories = append(categories, category{"autosync", "🔁 Auto Sync (Channel → Channel)", []string{
		"autosync", "asadd", "asaccount", "asrun", "astoggle", "astimes", "asreplace", "asreset", "asdel",
	}})
}

// LoadAutoSync — commands.Load() reflect ne auto call karto.
func (m *command) LoadAutoSync(d dispatcher.Dispatcher) {
	log := m.log.Named("autosync")
	defer log.Sugar().Info("Loaded AutoSync commands (/autosync /asadd /asaccount /asrun /astoggle /astimes /asreplace /asreset /asdel)")

	d.AddHandler(handlers.NewCommand("autosync", adminOnly(cmdASList)))
	d.AddHandler(handlers.NewCommand("asadd", adminOnly(cmdASAdd)))
	d.AddHandler(handlers.NewCommand("asaccount", adminOnly(cmdASAccount)))
	d.AddHandler(handlers.NewCommand("asrun", adminOnly(cmdASRun)))
	d.AddHandler(handlers.NewCommand("astoggle", adminOnly(cmdASToggle)))
	d.AddHandler(handlers.NewCommand("astimes", adminOnly(cmdASTimes)))
	d.AddHandler(handlers.NewCommand("asreplace", adminOnly(cmdASReplace)))
	d.AddHandler(handlers.NewCommand("asreset", adminOnly(cmdASReset)))
	d.AddHandler(handlers.NewCommand("asdel", adminOnly(cmdASDel)))

	// Group -3 — menu pending (-2) ani relay (-1) chya aadhi: wizard che plain-text replies.
	d.AddHandlerToGroup(handlers.NewMessage(nil, handleAutoSyncWizard), -3)
}
