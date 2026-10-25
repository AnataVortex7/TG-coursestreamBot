// internal/commands/autosync_progress.go
//
// AutoSync — live progress view (opt-in, spam nahi).
//
//   * Engine (autosync_engine.go) progress state fakt memory madhe thevto (swasta, DB write nahi).
//   * Admin "📊 Progress" button / /asprogress dabto → ek SNAPSHOT message (kahi auto-update nahi).
//   * "▶️ Live ON" dabla tarach ekach message ~5 sec ni edit hoto (navin message yet nahi).
//     "⏹ Live OFF" kiva task sampla kiva 3 tas zale ki aapoaap band.
package commands

import (
	"context"
	"errors"
	"fmt"
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

// ─── progress state (in-memory) ──────────────────────────────────────────────

type asProgress struct {
	Name       string
	Mode       string
	Started    time.Time
	Phase      string
	Total      int // ya run madhe process karayche messages
	Done       int // process zalele (copied + skipped + errors)
	Copied     int
	Skipped    int
	Errors     int
	Beyond     int // per-run cap mule pudhchya run la gelele
	CurMsgID   int
	CurDesc    string
	Sub        string // "⬇️ Download" / "⬆️ Upload" / "⚡ Fast copy"
	SubWritten int64
	SubTotal   int64
	Finished   bool
	Touched    time.Time // shevtcha progress (bytes / phase badal) — stall watchdog sathi
	GraceUntil time.Time // FloodWait/backoff madhe watchdog band
}

var (
	asProgMu  sync.Mutex
	asProgMap = map[string]*asProgress{}
)

func asProgStart(id, name, mode string) {
	asProgMu.Lock()
	asProgMap[id] = &asProgress{Name: name, Mode: mode, Started: time.Now(), Phase: "🔎 Source check hot ahe…"}
	asProgMu.Unlock()
}

func asProgUpdate(id string, fn func(p *asProgress)) {
	asProgMu.Lock()
	if p := asProgMap[id]; p != nil {
		fn(p)
		p.Touched = time.Now()
	}
	asProgMu.Unlock()
}

func asProgSub(id, label string, written, total int64) {
	asProgMu.Lock()
	defer asProgMu.Unlock()
	p := asProgMap[id]
	if p == nil {
		return
	}
	// Touch fakt jevha label badalla kiva bytes pudhe gele (real progress) — repeat call la nahi.
	if p.Sub != label || p.SubWritten != written || p.SubTotal != total {
		p.Touched = time.Now()
	}
	p.Sub, p.SubWritten, p.SubTotal = label, written, total
}

func asProgFinish(id string) {
	asProgUpdate(id, func(p *asProgress) {
		p.Finished = true
		p.Sub, p.CurDesc = "", ""
	})
}

func asProgSnapshot(id string) (asProgress, bool) {
	asProgMu.Lock()
	defer asProgMu.Unlock()
	p := asProgMap[id]
	if p == nil {
		return asProgress{}, false
	}
	return *p, true
}

// ─── formatting helpers ──────────────────────────────────────────────────────

func asHumanSize(n int64) string {
	const k = 1024.0
	f := float64(n)
	switch {
	case f >= k*k*k:
		return fmt.Sprintf("%.2f GB", f/(k*k*k))
	case f >= k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	case f >= k:
		return fmt.Sprintf("%.0f KB", f/k)
	}
	return fmt.Sprintf("%d B", n)
}

func asBar(done, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	if done > total {
		done = total
	}
	if done < 0 {
		done = 0
	}
	f := done * width / total
	return strings.Repeat("█", f) + strings.Repeat("░", width-f)
}

func asDur(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	if m >= 60 {
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	return fmt.Sprintf("%02d:%02d", m, int(d.Seconds())%60)
}

// asDescribeMsg — konti media/text ata jat ahe (progress screen sathi).
func asDescribeMsg(m *tg.Message) string {
	switch md := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		return "🖼 Photo"
	case *tg.MessageMediaDocument:
		if d, ok := md.Document.AsNotEmpty(); ok {
			kind, name := "📄 File", ""
			for _, a := range d.Attributes {
				switch at := a.(type) {
				case *tg.DocumentAttributeFilename:
					name = at.FileName
				case *tg.DocumentAttributeVideo:
					kind = "🎬 Video"
				case *tg.DocumentAttributeAudio:
					kind = "🎵 Audio"
				}
			}
			if name == "" {
				name = strings.TrimSpace(strings.SplitN(m.Message, "\n", 2)[0])
			}
			if name == "" {
				name = d.MimeType
			}
			return fmt.Sprintf("%s: %s (%s)", kind, clipRunes(name, 50), asHumanSize(d.Size))
		}
	}
	line := strings.TrimSpace(strings.SplitN(m.Message, "\n", 2)[0])
	if line == "" {
		return "💬 Message"
	}
	return "💬 Text: " + clipRunes(line, 50)
}

func asNextRun(t *db.AutoSyncTask, now time.Time) (time.Time, bool) {
	loc := istLoc()
	n := now.In(loc)
	var best time.Time
	found := false
	for _, hm := range t.Times {
		h, m, ok := asParseHM(hm)
		if !ok {
			continue
		}
		for day := 0; day <= 1; day++ {
			slot := time.Date(n.Year(), n.Month(), n.Day()+day, h, m, 0, 0, loc)
			if slot.After(n) {
				if !found || slot.Before(best) {
					best, found = slot, true
				}
				break
			}
		}
	}
	return best, found
}

// asRenderProgress — task cha current progress text.
func asRenderProgress(taskHex string, live bool) string {
	oid, err := primitiveObjectID(taskHex)
	if err != nil {
		return "❌ invalid task id"
	}
	t, _ := db.GetAutoSyncTask(oid)
	if t == nil {
		return "❌ Task sapadla nahi (delete zala asel)."
	}
	head := "📊 PROGRESS • ⚪ snapshot"
	if live {
		head = "📊 PROGRESS • 🟢 LIVE"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s\n🔁 %s\n📡 %s → 📥 %s\n\n", head, t.Name, clipRunes(t.SourceTitle, 28), clipRunes(t.DestTitle, 28)))

	_, running := asRunning.Load(taskHex)
	snap, has := asProgSnapshot(taskHex)
	if running && has {
		if snap.Total == 0 {
			sb.WriteString("▶️ RUNNING\n" + snap.Phase + "\n")
		} else {
			pct := snap.Done * 100 / snap.Total
			sb.WriteString(fmt.Sprintf("▶️ RUNNING (%s mode)\n[%s] %d%%  %d/%d\n", snap.Mode, asBar(snap.Done, snap.Total, 12), pct, snap.Done, snap.Total))
			sb.WriteString(fmt.Sprintf("✅ Copied %d | ⏭ Skipped %d | ❌ Errors %d\n", snap.Copied, snap.Skipped, snap.Errors))
			rem := snap.Total - snap.Done
			if rem < 0 {
				rem = 0
			}
			line := fmt.Sprintf("⏳ Baki: %d", rem)
			if snap.Beyond > 0 {
				line += fmt.Sprintf(" (+%d pudhchya run madhe)", snap.Beyond)
			}
			sb.WriteString(line + "\n")
			if snap.CurDesc != "" {
				sb.WriteString(fmt.Sprintf("\n🎯 Ata: %s\n    (source msg %d)\n", snap.CurDesc, snap.CurMsgID))
			}
			if snap.Sub != "" {
				if snap.SubTotal > 0 {
					sb.WriteString(fmt.Sprintf("%s %d%% — %s / %s\n", snap.Sub, int(snap.SubWritten*100/snap.SubTotal), asHumanSize(snap.SubWritten), asHumanSize(snap.SubTotal)))
				} else {
					sb.WriteString(snap.Sub + "…\n")
				}
			}
			el := time.Since(snap.Started)
			eta := ""
			if snap.Done > 0 && snap.Total > snap.Done {
				eta = " | ETA ~" + asDur(el/time.Duration(snap.Done)*time.Duration(snap.Total-snap.Done))
			}
			sb.WriteString(fmt.Sprintf("\n🕒 Elapsed %s%s\n", asDur(el), eta))
		}
	} else {
		sb.WriteString("⏸ Ata task chalu nahi\n")
		if nr, ok := asNextRun(t, time.Now()); ok {
			sb.WriteString("🕒 Next run: " + nr.In(istLoc()).Format("02 Jan, 15:04") + " IST\n")
		}
		if !t.Enabled {
			sb.WriteString("⚠️ Task PAUSED ahe (/astoggle)\n")
		}
		sb.WriteString(fmt.Sprintf("\n📝 Last run: %s\n📊 Ekun copied (sarve runs): %d\n", clipRunes(t.LastStatus, 300), t.TotalCopied))
	}
	return sb.String()
}

// ─── buttons ─────────────────────────────────────────────────────────────────

func asProgressMarkup(id string, live bool) tg.ReplyMarkupClass {
	btn := func(text, data string) tg.KeyboardButtonClass {
		return &tg.KeyboardButtonCallback{Text: text, Data: []byte(data)}
	}
	toggle := btn("▶️ Live ON", "asp:on:"+id)
	if live {
		toggle = btn("⏹ Live OFF", "asp:off:"+id)
	}
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{toggle, btn("🔄 Refresh", "asp:ref:"+id)}},
		{Buttons: []tg.KeyboardButtonClass{btn("▶️ Run Now + Live", "asp:run:"+id)}},
		{Buttons: []tg.KeyboardButtonClass{btn("⬅️ Tasks", "cmd:autosync")}},
	}}
}

func asEditProgress(adminID int64, msgID int, text string, markup tg.ReplyMarkupClass) error {
	if MainBotClient == nil {
		return errors.New("bot client nahi")
	}
	gctx := MainBotClient.CreateContext()
	_, err := gctx.EditMessage(adminID, &tg.MessagesEditMessageRequest{
		ID:          msgID,
		Message:     text,
		ReplyMarkup: markup,
	})
	return err
}

// ─── live watchers ───────────────────────────────────────────────────────────

type asWatch struct {
	cancel   context.CancelFunc
	replaced bool
}

var (
	asWatchMu sync.Mutex
	asWatches = map[string]*asWatch{}
)

const asLiveMax = 3 * time.Hour

func asWatchKey(adminID int64, taskHex string) string { return fmt.Sprintf("%d:%s", adminID, taskHex) }

// stopASWatch — true = watcher hota ani ata thambvla (to swata final edit karel).
func stopASWatch(adminID int64, taskHex string) bool {
	asWatchMu.Lock()
	defer asWatchMu.Unlock()
	w := asWatches[asWatchKey(adminID, taskHex)]
	if w == nil {
		return false
	}
	w.cancel()
	return true
}

func startASWatch(adminID int64, taskHex string, msgID int) {
	key := asWatchKey(adminID, taskHex)
	wctx, cancel := context.WithTimeout(context.Background(), asLiveMax)
	me := &asWatch{cancel: cancel}

	asWatchMu.Lock()
	if old := asWatches[key]; old != nil {
		old.replaced = true // junya watcher ne final edit karu naye
		old.cancel()
	}
	asWatches[key] = me
	asWatchMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Logger.Sugar().Errorw("autosync live watcher panic", "panic", r)
			}
		}()
		defer cancel()
		last := ""
		sawRunning := false
		failures := 0
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		finalize := func() {
			asWatchMu.Lock()
			if asWatches[key] == me {
				delete(asWatches, key)
			}
			skip := me.replaced
			asWatchMu.Unlock()
			if skip {
				return
			}
			_ = asEditProgress(adminID, msgID, asRenderProgress(taskHex, false), asProgressMarkup(taskHex, false))
		}

		for {
			_, running := asRunning.Load(taskHex)
			if running {
				sawRunning = true
			}
			if !running && sawRunning {
				finalize() // run sampla — final snapshot, live band
				return
			}
			text := asRenderProgress(taskHex, true)
			if text != last {
				err := asEditProgress(adminID, msgID, text, asProgressMarkup(taskHex, true))
				switch {
				case err == nil:
					last = text
					failures = 0
				case isMessageNotModifiedErr(err):
					last = text
				case strings.Contains(strings.ToUpper(err.Error()), "MESSAGE_ID_INVALID"):
					asWatchMu.Lock()
					if asWatches[key] == me {
						delete(asWatches, key)
					}
					asWatchMu.Unlock()
					return // message delete zala
				default:
					if sec, ok := utils.ExtractFloodWaitSeconds(err); ok {
						if sec > 60 {
							sec = 60
						}
						select {
						case <-time.After(time.Duration(sec+1) * time.Second):
						case <-wctx.Done():
						}
					}
					failures++
					if failures >= 8 {
						finalize()
						return
					}
				}
			}
			select {
			case <-wctx.Done():
				finalize()
				return
			case <-ticker.C:
			}
		}
	}()
}

// ─── command + callbacks ─────────────────────────────────────────────────────

func cmdASProgress(ctx *ext.Context, u *ext.Update) error {
	var t *db.AutoSyncTask
	if len(args(u)) == 0 {
		tasks, _ := db.ListAutoSyncTasks()
		switch len(tasks) {
		case 0:
			asReplyWithButtons(ctx, u, "🔁 Ajun ekahi task nahi.")
			return dispatcher.EndGroups
		case 1:
			t = &tasks[0]
		default:
			reply(ctx, u, "Task ID dya: /asprogress <id>\n(IDs: /autosync) kiva menu madhun 📊 Progress.")
			return dispatcher.EndGroups
		}
	} else {
		t, _ = asTaskFromArgs(ctx, u)
		if t == nil {
			return dispatcher.EndGroups
		}
	}
	id := t.ID.Hex()
	text := asRenderProgress(id, false)
	markup := asProgressMarkup(id, false)
	if u.CallbackQuery != nil {
		editMenu(ctx, u, text, markup)
		return dispatcher.EndGroups
	}
	_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
	return dispatcher.EndGroups
}

func handleASProgressCb(ctx *ext.Context, u *ext.Update) error {
	if u.CallbackQuery == nil || u.EffectiveUser() == nil {
		return dispatcher.ContinueGroups
	}
	_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID})
	adminID := u.EffectiveUser().GetID()
	if !utils.Contains(config.ValueOf.AllowedUsers, adminID) {
		return dispatcher.EndGroups
	}
	parts := strings.Split(string(u.CallbackQuery.Data), ":")
	if len(parts) != 3 {
		return dispatcher.EndGroups
	}
	act, id := parts[1], parts[2]
	msgID := u.CallbackQuery.MsgID
	if _, err := primitiveObjectID(id); err != nil {
		return dispatcher.EndGroups
	}

	switch act {
	case "on":
		startASWatch(adminID, id, msgID)
	case "off":
		if !stopASWatch(adminID, id) { // watcher nahi (restart zala) — direct snapshot
			_ = asEditProgress(adminID, msgID, asRenderProgress(id, false), asProgressMarkup(id, false))
		}
	case "ref":
		asWatchMu.Lock()
		_, live := asWatches[asWatchKey(adminID, id)]
		asWatchMu.Unlock()
		if !live {
			_ = asEditProgress(adminID, msgID, asRenderProgress(id, false), asProgressMarkup(id, false))
		}
	case "run":
		if _, running := asRunning.Load(id); !running {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						utils.Logger.Sugar().Errorw("autosync run (progress btn) panic", "panic", r)
						asRunning.Delete(id)
					}
				}()
				runAutoSyncTask(asBaseCtx(), id, true)
			}()
			time.Sleep(1500 * time.Millisecond) // run register hou dya
		}
		startASWatch(adminID, id, msgID)
	}
	return dispatcher.EndGroups
}

func init() {
	registerCmd(menuCmd{key: "asprogress", label: "📊 Progress (Live)", kind: kindFlow, fn: adminOnly(cmdASProgress), steps: []cmdStep{
		selectStep("📊 Konatya task cha progress pahaicha?", listASTaskItems),
	}})
}

// LoadAutoSyncProgress — commands.Load() reflect ne auto call karto.
func (m *command) LoadAutoSyncProgress(d dispatcher.Dispatcher) {
	log := m.log.Named("autosync-progress")
	defer log.Sugar().Info("Loaded AutoSync progress (/asprogress + live buttons)")
	d.AddHandler(handlers.NewCommand("asprogress", adminOnly(cmdASProgress)))
	d.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("asp:"), handleASProgressCb))
}
