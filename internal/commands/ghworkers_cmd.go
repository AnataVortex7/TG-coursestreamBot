// internal/commands/ghworkers_cmd.go
//
// /cmd → ☁️ Colab Worker मधले GitHub Actions workspace buttons.
// Handlers reflection (Load*) ने auto-register होतात; menu entries init() मधून.
package commands

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/ghworkers"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
)

func (m *command) LoadGHWorkers(d dispatcher.Dispatcher) {
	for key, fn := range map[string]func(*ext.Context, *ext.Update) error{
		"ghstatus": cmdGHStatus, "ghon": cmdGHOn, "ghoff": cmdGHOff,
		"ghadd": cmdGHAdd, "ghdel": cmdGHDel, "ghtoggle": cmdGHToggle,
		"ghstart": cmdGHStart, "ghstop": cmdGHStop,
		"ghrestartwait": cmdGHRestartWait, "ghparallel": cmdGHParallel, "ghwsdaily": cmdGHWsDaily, "ghidle": cmdGHIdle, "ghdaily": cmdGHDaily, "ghrunlimit": cmdGHRunLimit,
	} {
		d.AddHandler(handlers.NewCommand(key, adminOnly(fn)))
	}
}

func init() {
	registerCmd(menuCmd{key: "ghstatus", label: "🐙 GitHub Workspaces Status", kind: kindView, fn: cmdGHStatus})
	registerCmd(menuCmd{key: "ghon", label: "✅ GitHub Auto ON", kind: kindToggle, fn: cmdGHOn})
	registerCmd(menuCmd{key: "ghoff", label: "⛔ GitHub Auto OFF", kind: kindToggle, fn: cmdGHOff})
	registerCmd(menuCmd{key: "ghadd", label: "➕ Add GitHub Workspace", kind: kindFlow, fn: cmdGHAdd, steps: []cmdStep{
		inputStep("📦 Repo टाका — `owner/repo` (उदा: `akshaym118/watermark-server`):"),
		inputStep("🔑 त्या GitHub account चा token टाका (Actions + Contents write):\n⚠️ Add झाल्यावर हा message chat मधून delete करा."),
	}})
	registerCmd(menuCmd{key: "ghtoggle", label: "🎯 Select Workspaces (ON/OFF)", kind: kindFlow, fn: cmdGHToggle, steps: []cmdStep{
		selectStep("🎯 Watermark साठी वापरायचा workspace — निवडा (ON↔OFF होईल):", ghListItems(false)),
	}})
	registerCmd(menuCmd{key: "ghdel", label: "🗑 Remove GitHub Workspace", kind: kindFlow, fn: cmdGHDel, steps: []cmdStep{
		selectStep("🗑 कोणता workspace काढायचा?", ghListItems(false)),
	}})
	registerCmd(menuCmd{key: "ghstart", label: "▶️ GitHub Worker Start", kind: kindFlow, fn: cmdGHStart, steps: []cmdStep{
		selectStep("▶️ कोणता start करायचा?", ghListItems(true)),
	}})
	registerCmd(menuCmd{key: "ghstop", label: "⏹ GitHub Worker Stop", kind: kindFlow, fn: cmdGHStop, steps: []cmdStep{
		selectStep("⏹ कोणता stop करायचा?", ghListItems(true)),
	}})
	registerCmd(menuCmd{key: "ghrestartwait", label: "⏱ Restart Wait (min)", kind: kindFlow, fn: cmdGHRestartWait, steps: []cmdStep{
		inputStep("⏱ Run limit नंतर किती मिनिटे थांबून restart करायचं? (1–240, default 20):"),
	}})
	registerCmd(menuCmd{key: "ghrunlimit", label: "⏳ Max Run Hours", kind: kindFlow, fn: cmdGHRunLimit, steps: []cmdStep{
		inputStep("⏳ एका run ची कमाल वेळ तासांत (0.5–5.9, default 5):"),
	}})
	registerCmd(menuCmd{key: "ghparallel", label: "⚡ Parallel Workspaces", kind: kindFlow, fn: cmdGHParallel, steps: []cmdStep{
		inputStep("⚡ एकाच वेळी किती workspaces (वेगळे accounts) चालवायचे? प्रत्येकाला GitHub चा पूर्ण runner मिळतो. (1–10, default 2):"),
	}})
	registerCmd(menuCmd{key: "ghidle", label: "💤 Idle Kill (min)", kind: kindFlow, fn: cmdGHIdle, steps: []cmdStep{
		inputStep("💤 Jobs संपल्यावर किती मिनिटांनी workflow kill करायचा? (0 = लगेच, default 1):"),
	}})
	registerCmd(menuCmd{key: "ghwsdaily", label: "⏱ Workspace Daily Timer", kind: kindFlow, fn: cmdGHWsDaily, steps: []cmdStep{
		selectStep("⏱ कोणत्या workspace चा daily timer बदलायचा?", ghListItems(false)),
		inputStep("⏱ त्या workspace ने दिवसाला कमाल किती तास चालायचं? (0.5–24; `0` = default वापर):"),
	}})
	registerCmd(menuCmd{key: "ghdaily", label: "📅 Default Daily Hours (सगळ्यांसाठी)", kind: kindFlow, fn: cmdGHDaily, steps: []cmdStep{
		inputStep("📅 प्रत्येक workspace ला दिवसाला कमाल किती तास? (0.5–24, default 10):"),
	}})

	for i := range categories {
		if categories[i].key == "colab" {
			categories[i].cmdKeys = append(categories[i].cmdKeys,
				"ghstatus", "ghon", "ghoff", "ghadd", "ghtoggle", "ghstart", "ghstop",
				"ghrestartwait", "ghparallel", "ghwsdaily", "ghidle", "ghrunlimit", "ghdaily", "ghdel")
		}
	}
}

// ghListItems — select list. withAll=true → वर "सगळे" पर्याय.
func ghListItems(withAll bool) func() ([]listItem, string, error) {
	return func() ([]listItem, string, error) {
		snap, err := ghworkers.GetSnapshot()
		if err != nil {
			return nil, "", err
		}
		var items []listItem
		if withAll && len(snap.Workspaces) > 0 {
			items = append(items, listItem{display: "🌐 सगळे (Enabled)", id: "all"})
		}
		for _, w := range snap.Workspaces {
			mark := "⚪"
			if w.Enabled {
				mark = "🟢"
			}
			items = append(items, listItem{display: fmt.Sprintf("%s %s — %s [%s]", mark, w.Label, w.Repo, w.State), id: w.ID})
		}
		return items, "❌ एकही workspace add नाही. आधी ➕ Add GitHub Workspace वापरा.", nil
	}
}

func ghStateIcon(s string) string {
	switch s {
	case "running":
		return "🟢 running"
	case "queued":
		return "🟡 queued"
	case "cooldown":
		return "⏸ cooldown"
	case "limit":
		return "🚫 daily limit"
	case "blocked":
		return "⛔ blocked (error)"
	case "disabled":
		return "⚪ not selected"
	}
	return "🔵 ready"
}

func cmdGHStatus(ctx *ext.Context, u *ext.Update) error {
	snap, err := ghworkers.GetSnapshot()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	s := snap.Settings
	var b strings.Builder
	fmt.Fprintf(&b, "🐙 GitHub Workspaces\n\nAuto: %s\nQueue: %d pending, %d processing\nLimits: parallel workspaces %d • run %.1fh • default daily %.1fh • restart wait %d min • idle kill %d min\n",
		map[bool]string{true: "🟢 ON", false: "🔴 OFF"}[s.AutoEnabled], snap.Pending, snap.Processing,
		s.Parallel, float64(s.RunLimitMin)/60, float64(s.DailyLimitMin)/60, s.RestartWaitMin, s.IdleOffMin)
	if len(snap.Workspaces) == 0 {
		b.WriteString("\n(एकही workspace नाही)")
	}
	for _, w := range snap.Workspaces {
		fmt.Fprintf(&b, "\n• %s (%s)\n  %s | आज %d/%d min%s", w.Label, w.Repo, ghStateIcon(w.State), w.UsedMin, w.DailyLimitMin, map[bool]string{true: " (custom)", false: ""}[w.CustomDaily])
		if w.State == "running" {
			fmt.Fprintf(&b, " | run %d min", w.RunElapsedMin)
		}
		if w.State == "blocked" {
			b.WriteString("\n  ⛔ " + w.BlockedReason + "\n  (ठीक केल्यावर ▶️ Start दाबा)")
		}
		if w.Runs > 0 {
			fmt.Fprintf(&b, " | %d run(s)", w.Runs)
		}
		if w.State == "cooldown" {
			fmt.Fprintf(&b, " | restart ~%d min", w.CooldownLeftMin)
		}
		if w.LastError != "" {
			b.WriteString("\n  ⚠️ " + w.LastError)
		}
	}
	reply(ctx, u, b.String())
	return dispatcher.EndGroups
}

func ghSetAuto(ctx *ext.Context, u *ext.Update, on bool) error {
	if err := ghworkers.SaveSettings(&on, nil, nil, nil, nil, nil); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else if on {
		reply(ctx, u, "✅ GitHub auto start/stop ON\n\nWatermark job आला की निवडलेले सगळे workspaces आपोआप start होतील; काम संपल्यावर off.")
	} else {
		reply(ctx, u, "⛔ GitHub auto OFF — आता workspaces आपोआप start/stop होणार नाहीत (चालू runs तसेच राहतील).")
	}
	return dispatcher.EndGroups
}
func cmdGHOn(ctx *ext.Context, u *ext.Update) error  { return ghSetAuto(ctx, u, true) }
func cmdGHOff(ctx *ext.Context, u *ext.Update) error { return ghSetAuto(ctx, u, false) }

func cmdGHAdd(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "वापर: /ghadd owner/repo TOKEN")
		return dispatcher.EndGroups
	}
	w, err := ghworkers.AddWorkspace("", a[0], a[1], "")
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u, fmt.Sprintf("✅ Workspace add झाला: %s/%s\n\n⚠️ Token असलेला message chat मधून आत्ताच delete करा.\nAuto ON करायला: ✅ GitHub Auto ON", w.Owner, w.Repo))
	return dispatcher.EndGroups
}

func ghFirstArg(ctx *ext.Context, u *ext.Update) (string, bool) {
	a := args(u)
	if len(a) == 0 {
		reply(ctx, u, "❌ workspace निवडा (/cmd मधून button वापरा)")
		return "", false
	}
	return a[0], true
}

func cmdGHDel(ctx *ext.Context, u *ext.Update) error {
	id, ok := ghFirstArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	if err := ghworkers.DeleteWorkspace(id); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, "🗑 Workspace काढला (चालू run असेल तर cancel केला).")
	}
	return dispatcher.EndGroups
}

func cmdGHToggle(ctx *ext.Context, u *ext.Update) error {
	id, ok := ghFirstArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	snap, err := ghworkers.GetSnapshot()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	for _, w := range snap.Workspaces {
		if w.ID == id {
			if err := ghworkers.SetEnabled(id, !w.Enabled); err != nil {
				reply(ctx, u, "❌ "+err.Error())
			} else if !w.Enabled {
				reply(ctx, u, "🟢 "+w.Label+" — watermark साठी निवडला (ON)")
			} else {
				reply(ctx, u, "⚪ "+w.Label+" — निवड काढली (OFF), चालू run असेल तर बंद केला")
			}
			return dispatcher.EndGroups
		}
	}
	reply(ctx, u, "❌ workspace सापडला नाही")
	return dispatcher.EndGroups
}

func ghRunAction(ctx *ext.Context, u *ext.Update, one func(string) error, all func() (int, []string), verb string) error {
	id, ok := ghFirstArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	if id == "all" {
		n, errs := all()
		msg := fmt.Sprintf("%s: %d workspace(s)", verb, n)
		if len(errs) > 0 {
			msg += "\n⚠️ " + strings.Join(errs, "\n⚠️ ")
		}
		reply(ctx, u, msg)
		return dispatcher.EndGroups
	}
	if err := one(id); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, "✅ "+verb)
	}
	return dispatcher.EndGroups
}

func cmdGHStart(ctx *ext.Context, u *ext.Update) error {
	return ghRunAction(ctx, u, ghworkers.StartNow, ghworkers.StartAll, "🚀 start पाठवला")
}
func cmdGHStop(ctx *ext.Context, u *ext.Update) error {
	return ghRunAction(ctx, u, ghworkers.StopNow, ghworkers.StopAll, "🛑 stop पाठवला")
}

func ghNumArg(ctx *ext.Context, u *ext.Update) (float64, bool) {
	a := args(u)
	if len(a) == 0 {
		reply(ctx, u, "❌ संख्या टाका")
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(a[0], ",", "."), 64)
	if err != nil || v <= 0 {
		reply(ctx, u, "❌ valid संख्या टाका")
		return 0, false
	}
	return v, true
}

func cmdGHRestartWait(ctx *ext.Context, u *ext.Update) error {
	v, ok := ghNumArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	m := int64(v)
	if err := ghworkers.SaveSettings(nil, nil, nil, &m, nil, nil); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, fmt.Sprintf("⏱ Restart wait = %d min", m))
	}
	return dispatcher.EndGroups
}

func cmdGHRunLimit(ctx *ext.Context, u *ext.Update) error {
	v, ok := ghNumArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	m := int64(v * 60)
	if err := ghworkers.SaveSettings(nil, nil, &m, nil, nil, nil); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, fmt.Sprintf("⏳ Max run = %.1f h (%v)", v, time.Duration(m)*time.Minute))
	}
	return dispatcher.EndGroups
}

func cmdGHDaily(ctx *ext.Context, u *ext.Update) error {
	v, ok := ghNumArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	m := int64(v * 60)
	if err := ghworkers.SaveSettings(nil, &m, nil, nil, nil, nil); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, fmt.Sprintf("📅 Daily limit = %.1f h प्रति workspace", v))
	}
	return dispatcher.EndGroups
}

func cmdGHParallel(ctx *ext.Context, u *ext.Update) error {
	v, ok := ghNumArg(ctx, u)
	if !ok {
		return dispatcher.EndGroups
	}
	m := int64(v)
	if err := ghworkers.SaveSettings(nil, nil, nil, nil, nil, &m); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, fmt.Sprintf("⚡ Parallel = %d runs प्रति workspace", m))
	}
	return dispatcher.EndGroups
}

func cmdGHIdle(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	v, err := strconv.ParseInt(strings.TrimSpace(strings.Join(a, "")), 10, 64)
	if err != nil || v < 0 {
		reply(ctx, u, "❌ 0 किंवा जास्त पूर्ण संख्या टाका")
		return dispatcher.EndGroups
	}
	if err := ghworkers.SaveSettings(nil, nil, nil, nil, &v, nil); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else {
		reply(ctx, u, fmt.Sprintf("💤 Idle kill = %d min (jobs संपल्यावर)", v))
	}
	return dispatcher.EndGroups
}

func cmdGHWsDaily(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "वापर: /ghwsdaily <workspace_id> <तास>  (0 = default)")
		return dispatcher.EndGroups
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(a[1], ",", "."), 64)
	if err != nil || v < 0 {
		reply(ctx, u, "❌ valid तास टाका (उदा: 6 किंवा 7.5, default साठी 0)")
		return dispatcher.EndGroups
	}
	if err := ghworkers.SetDailyLimit(a[0], int64(v*60)); err != nil {
		reply(ctx, u, "❌ "+err.Error())
	} else if v == 0 {
		reply(ctx, u, "⏱ त्या workspace ला आता default daily timer लागू")
	} else {
		reply(ctx, u, fmt.Sprintf("⏱ Workspace daily timer = %.1f h", v))
	}
	return dispatcher.EndGroups
}
