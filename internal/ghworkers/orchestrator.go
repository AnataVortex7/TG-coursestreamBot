// Package ghworkers — GitHub Actions workspaces वर watermark worker auto start/stop.
//
// नियम (सगळे admin ने बदलता येतात):
//   - प्रत्येक workspace = वेगळं GitHub account/repo = एक स्वतंत्र run (त्याला GitHub चा पूर्ण runner/compute).
//   - Watermark queue मध्ये pending job आला की निवडलेले (Enabled) workspaces start; एकाच वेळी
//     कमाल gh_parallel (default 2) workspaces चालतात (ज्याचा आजचा वापर कमी तो आधी).
//   - Jobs संपले की gh_idle_off_min (default 1, 0 = लगेच) नंतर सगळे runs kill.
//   - एक run कमाल gh_run_limit_min (default 5h) → cancel → gh_restart_wait_min (default 20) थांबून restart.
//   - प्रत्येक workspace ला दिवसाला कमाल वेळ: workspace चा स्वतःचा timer (bot/panel मधून), नसेल तर global gh_daily_limit_min (IST दिवस).
//   - GitHub/account error (401/403/404/422, token/permission/workflow चुकीचा) किंवा workflow
//     सलग 2 वेळा लगेच fail → त्या workspace वर run सुरू करणं बंद (admin ने Start/Use देऊन clear करेपर्यंत).
//     5xx / rate-limit → 10 min थांबतो.
package ghworkers

import (
	"errors"
	"sort"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/google/uuid"
)

const (
	tickEvery       = 45 * time.Second
	minDispatchGap  = 3 * time.Minute
	minUsefulWindow = 15 * time.Minute
	quickFail       = 5 * time.Minute
	defaultEvent    = "start_worker"
)

var (
	opMu       sync.Mutex
	startOnce  sync.Once
	instanceID = uuid.NewString()
	ist        = time.FixedZone("IST", 5*3600+1800)
	forever    = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ── Settings ────────────────────────────────────────────────────────────────

type Settings struct {
	AutoEnabled    bool  `json:"auto_enabled"`
	DailyLimitMin  int64 `json:"daily_limit_min"`
	RunLimitMin    int64 `json:"run_limit_min"`
	RestartWaitMin int64 `json:"restart_wait_min"`
	IdleOffMin     int64 `json:"idle_off_min"`
	Parallel       int64 `json:"parallel"`
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func LoadSettings() Settings {
	return Settings{
		AutoEnabled:    db.GetSettingBool(db.SettingGHAutoEnabled, false),
		DailyLimitMin:  clamp(db.GetSettingInt64(db.SettingGHDailyLimitMin, 600), 30, 1440),
		RunLimitMin:    clamp(db.GetSettingInt64(db.SettingGHRunLimitMin, 300), 30, 355),
		RestartWaitMin: clamp(db.GetSettingInt64(db.SettingGHRestartWaitMn, 20), 1, 240),
		IdleOffMin:     clamp(db.GetSettingInt64(db.SettingGHIdleOffMin, 1), 0, 120),
		Parallel:       clamp(db.GetSettingInt64(db.SettingGHParallel, 2), 1, 10),
	}
}

// SaveSettings — nil नसलेली fields बदलतो.
func SaveSettings(auto *bool, dailyMin, runMin, waitMin, idleMin, parallel *int64) error {
	if auto != nil {
		if err := db.SetSettingBool(db.SettingGHAutoEnabled, *auto); err != nil {
			return err
		}
	}
	set := func(key string, v *int64, lo, hi int64) error {
		if v == nil {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("%s: %d ते %d च्या दरम्यान हवं", key, lo, hi)
		}
		return db.SetSettingInt64(key, *v)
	}
	for _, e := range []error{
		set(db.SettingGHDailyLimitMin, dailyMin, 30, 1440),
		set(db.SettingGHRunLimitMin, runMin, 30, 355),
		set(db.SettingGHRestartWaitMn, waitMin, 1, 240),
		set(db.SettingGHIdleOffMin, idleMin, 0, 120),
		set(db.SettingGHParallel, parallel, 1, 10),
	} {
		if e != nil {
			return e
		}
	}
	return nil
}

func (s Settings) daily() time.Duration { return time.Duration(s.DailyLimitMin) * time.Minute }

// dailyFor — workspace चा स्वतःचा daily timer असेल तर तो, नाहीतर global.
func (s Settings) dailyFor(ws *db.GHWorkspace) time.Duration {
	if ws.DailyLimitMin > 0 {
		return time.Duration(ws.DailyLimitMin) * time.Minute
	}
	return s.daily()
}
func (s Settings) run() time.Duration   { return time.Duration(s.RunLimitMin) * time.Minute }
func (s Settings) wait() time.Duration  { return time.Duration(s.RestartWaitMin) * time.Minute }
func (s Settings) idle() time.Duration  { return time.Duration(s.IdleOffMin) * time.Minute }

func istDay(t time.Time) string { return t.In(ist).Format("2006-01-02") }

// ── Loop ────────────────────────────────────────────────────────────────────

func Start() {
	startOnce.Do(func() {
		go func() {
			time.Sleep(20 * time.Second)
			t := time.NewTicker(tickEvery)
			defer t.Stop()
			for range t.C {
				if db.Database == nil || !db.AcquireGHLease(instanceID, 2*tickEvery+15*time.Second) {
					continue
				}
				safeTick()
			}
		}()
		log.Printf("[ghworkers] orchestrator started (instance %s)", instanceID[:8])
	})
}

func safeTick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ghworkers] tick panic: %v", r)
		}
	}()
	tick()
}

func tick() {
	st := LoadSettings()
	if !st.AutoEnabled {
		return
	}
	opMu.Lock()
	defer opMu.Unlock()

	list, err := db.ListGHWorkspaces()
	if err != nil {
		log.Printf("[ghworkers] list failed: %v", err)
		return
	}
	pending := wmqueue.Default.PendingCount()
	work := pending + wmqueue.Default.ProcessingCount()
	now := time.Now()
	var synced []*db.GHWorkspace
	for i := range list {
		if reconcile(&list[i], st, work, now) {
			synced = append(synced, &list[i])
		}
	}
	if pending > 0 {
		startWithinCap(list, synced, st, pending, now)
	}
}

// startWithinCap — एकाच वेळी कमाल st.Parallel workspaces; ज्याचा आजचा वापर कमी तो आधी.
func startWithinCap(all []db.GHWorkspace, synced []*db.GHWorkspace, st Settings, pending int, now time.Time) {
	active := 0
	for i := range all {
		for _, rs := range all[i].Runs {
			if rs.CancelAskedAt.IsZero() {
				active++
				break
			}
		}
	}
	slots := int(st.Parallel) - active
	if slots <= 0 {
		return
	}
	sort.SliceStable(synced, func(i, j int) bool { return synced[i].UsedSeconds < synced[j].UsedSeconds })
	for _, ws := range synced {
		if slots <= 0 {
			break
		}
		if len(ws.Runs) > 0 {
			continue // आधी चालू (किंवा cancel होत असलेला) run — एका workspace वर एकच
		}
		token, err := decryptToken(ws.TokenEnc)
		if err != nil {
			continue
		}
		if maybeStart(ws, token, st, now) {
			slots--
			log.Printf("[ghworkers] %s/%s: dispatched (pending=%d)", ws.Owner, ws.Repo, pending)
		}
		_ = db.SaveGHWorkspaceRuntime(ws)
	}
}

// ── Error handling ──────────────────────────────────────────────────────────

// handleErr — GitHub/account error आला: fatal असेल तर workspace block, temporary असेल तर 10 min थांब.
func handleErr(ws *db.GHWorkspace, err error, now time.Time) {
	ws.LastError = err.Error()
	var ae *apiError
	if !errors.As(err, &ae) {
		return // network blip — पुढच्या tick ला पुन्हा प्रयत्न, block नाही
	}
	lower := strings.ToLower(ae.Msg)
	switch {
	case ae.Code >= 500 || ae.Code == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "abuse"):
		ws.BlockedUntil = now.Add(10 * time.Minute)
		ws.BlockedReason = "GitHub temporary error — 10 min थांबतोय: " + err.Error()
	case ae.Code == 401 || ae.Code == 403 || ae.Code == 404 || ae.Code == 422:
		ws.BlockedUntil = forever
		ws.BlockedReason = "GitHub/account error — बंद केलं (token/permission/repo/workflow तपासा, मग Start दाबा): " + err.Error()
	default:
		return
	}
	ws.LastEvent = "⛔ " + ws.BlockedReason
	log.Printf("[ghworkers] %s/%s blocked: %s", ws.Owner, ws.Repo, ws.BlockedReason)
}

func isBlocked(ws *db.GHWorkspace, now time.Time) bool { return now.Before(ws.BlockedUntil) }

func clearBlock(ws *db.GHWorkspace) {
	ws.BlockedUntil, ws.BlockedReason, ws.FailStreak = time.Time{}, "", 0
}

// ── Reconcile ───────────────────────────────────────────────────────────────

// reconcile — true = GitHub शी sync झालं (start साठी eligible).
func reconcile(ws *db.GHWorkspace, st Settings, work int, now time.Time) bool {
	if today := istDay(now); ws.UsageDay != today {
		ws.UsageDay, ws.UsedSeconds = today, 0
	}
	defer func() { _ = db.SaveGHWorkspaceRuntime(ws) }()

	if len(ws.Runs) == 0 && (!ws.Enabled || work == 0 || isBlocked(ws, now)) {
		return false // GitHub API call वाचवतो
	}
	token, err := decryptToken(ws.TokenEnc)
	if err != nil {
		ws.LastError = "token decrypt failed — workspace पुन्हा add करा"
		return false
	}
	runs, err := listRuns(token, ws.Owner, ws.Repo)
	if err != nil {
		handleErr(ws, err, now)
		return false
	}
	ws.LastError = ""

	active := map[int64]ghRun{}
	for _, r := range runs {
		if isActive(r.Status) {
			active[r.ID] = r
		}
	}

	// tracked runs: चालू राहिले का / संपले?
	var kept []db.GHRunState
	tracked := map[int64]bool{}
	for _, rs := range ws.Runs {
		r, ok := active[rs.ID]
		if !ok {
			finalize(ws, token, st, rs, now)
			continue
		}
		if rs.Start.IsZero() && r.Status == "in_progress" {
			rs.Start = r.RunStartedAt
			if rs.Start.IsZero() {
				rs.Start = now
			}
		}
		if rs.SeenAt.IsZero() {
			rs.SeenAt = now
		}
		kept = append(kept, rs)
		tracked[rs.ID] = true
	}
	// manual/अनोळखी active runs adopt
	for _, r := range runs {
		if _, ok := active[r.ID]; ok && !tracked[r.ID] {
			rs := db.GHRunState{ID: r.ID, SeenAt: now}
			if r.Status == "in_progress" {
				if rs.Start = r.RunStartedAt; rs.Start.IsZero() {
					rs.Start = now
				}
			}
			kept = append(kept, rs)
		}
	}
	ws.Runs = kept

	manageRuns(ws, token, st, work, now)
	return true
}

// finalize — run संपला: मिनिटे daily budget मध्ये; limit मुळे संपला तर cooldown; लगेच fail झाला तर block.
func finalize(ws *db.GHWorkspace, token string, st Settings, rs db.GHRunState, now time.Time) {
	end, conclusion := now, ""
	if r, err := getRun(token, ws.Owner, ws.Repo, rs.ID); err == nil {
		conclusion = r.Conclusion
		if !r.UpdatedAt.IsZero() && r.UpdatedAt.Before(now) {
			end = r.UpdatedAt
		}
	}
	var d time.Duration
	if !rs.Start.IsZero() {
		if d = end.Sub(rs.Start); d < 0 {
			d = 0
		}
		ws.UsedSeconds += int64(d.Seconds())
	}
	if d >= st.run()-90*time.Second && ws.CooldownUntil.Before(end.Add(st.wait())) {
		ws.CooldownUntil = end.Add(st.wait())
	}
	switch conclusion {
	case "failure", "startup_failure":
		if d < quickFail {
			ws.FailStreak++
		} else {
			ws.FailStreak = 0
		}
		if ws.FailStreak >= 2 {
			ws.BlockedUntil = forever
			ws.BlockedReason = "Workflow सलग 2 वेळा लगेच fail झाला — बंद केलं. Actions logs तपासा, मग Start दाबा."
			ws.LastEvent = "⛔ " + ws.BlockedReason
			return
		}
	case "success", "cancelled", "timed_out":
		ws.FailStreak = 0
	}
	ws.LastEvent = fmt.Sprintf("✅ run #%d संपला (%d min%s)", rs.ID, int(d.Minutes()), map[bool]string{true: ", " + conclusion, false: ""}[conclusion != ""])
}

func manageRuns(ws *db.GHWorkspace, token string, st Settings, work int, now time.Time) {
	if len(ws.Runs) == 0 {
		ws.IdleSince = time.Time{}
		return
	}
	idleStop := false
	if work == 0 {
		if ws.IdleSince.IsZero() {
			ws.IdleSince = now
		}
		idleStop = now.Sub(ws.IdleSince) >= st.idle()
	} else {
		ws.IdleSince = time.Time{}
	}

	used := time.Duration(ws.UsedSeconds) * time.Second
	for _, rs := range ws.Runs {
		if !rs.Start.IsZero() {
			used += now.Sub(rs.Start)
		}
	}

	kept := 0
	for i := range ws.Runs {
		rs := &ws.Runs[i]
		if !rs.CancelAskedAt.IsZero() { // cancel मागितला होता, run अजून चालू → force-cancel
			if now.Sub(rs.CancelAskedAt) > 2*time.Minute {
				if err := cancelRun(token, ws.Owner, ws.Repo, rs.ID, true); err != nil {
					ws.LastError = err.Error()
				}
				rs.CancelAskedAt = now
			}
			continue
		}
		queued := rs.Start.IsZero()
		kept++
		switch {
		case kept > 1:
			stopRun(ws, token, rs, "एका workspace वर एकच run (extra)", 0, now)
		case !ws.Enabled:
			stopRun(ws, token, rs, "workspace disabled", 0, now)
		case idleStop:
			stopRun(ws, token, rs, "jobs संपले", 0, now)
		case queued:
			// runner मिळायची वाट — फार वेळ अडकला तर काढ
			if !rs.SeenAt.IsZero() && now.Sub(rs.SeenAt) > 20*time.Minute {
				stopRun(ws, token, rs, "queue मध्ये 20 min अडकला", 0, now)
			}
		case now.Sub(rs.Start) >= st.run():
			stopRun(ws, token, rs, fmt.Sprintf("run limit %d min पूर्ण", st.RunLimitMin), st.wait(), now)
		case used >= st.dailyFor(ws):
			stopRun(ws, token, rs, fmt.Sprintf("daily limit %d min पूर्ण", int(st.dailyFor(ws).Minutes())), 0, now)
		}
	}
}

func stopRun(ws *db.GHWorkspace, token string, rs *db.GHRunState, reason string, cooldown time.Duration, now time.Time) {
	if err := cancelRun(token, ws.Owner, ws.Repo, rs.ID, false); err != nil {
		handleErr(ws, err, now)
		return
	}
	rs.CancelAskedAt = now
	if cooldown > 0 {
		ws.CooldownUntil = now.Add(cooldown)
	}
	ws.LastEvent = fmt.Sprintf("🛑 %s — run #%d kill", reason, rs.ID)
	log.Printf("[ghworkers] %s/%s: %s", ws.Owner, ws.Repo, ws.LastEvent)
}

func maybeStart(ws *db.GHWorkspace, token string, st Settings, now time.Time) bool {
	if !ws.Enabled || isBlocked(ws, now) || len(ws.Runs) > 0 {
		return false
	}
	if now.Before(ws.CooldownUntil) || now.Sub(ws.LastDispatchAt) < minDispatchGap {
		return false
	}
	if st.dailyFor(ws)-time.Duration(ws.UsedSeconds)*time.Second < minUsefulWindow {
		ws.LastEvent = "⏳ daily limit पूर्ण — उद्या IST 00:00 नंतर"
		return false
	}
	ws.LastDispatchAt = now
	if err := dispatch(token, ws.Owner, ws.Repo, eventOf(ws)); err != nil {
		handleErr(ws, err, now) // error आला → हा workspace थांबवतो
		return false
	}
	ws.IdleSince = time.Time{}
	ws.LastEvent = "🚀 worker start पाठवला"
	return true
}

func eventOf(ws *db.GHWorkspace) string {
	if ws.EventType != "" {
		return ws.EventType
	}
	return defaultEvent
}

// ── Manual operations (admin panel / Telegram) ──────────────────────────────

func ParseOwnerRepo(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		s = strings.Trim(u.Path, "/")
	}
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	p := strings.Split(s, "/")
	if len(p) != 2 || p[0] == "" || p[1] == "" || strings.ContainsAny(s, " ?#") {
		return "", "", fmt.Errorf("repo `owner/repo` या स्वरूपात द्या")
	}
	return p[0], p[1], nil
}

func AddWorkspace(label, ownerRepo, token, eventType string) (*db.GHWorkspace, error) {
	owner, repo, err := ParseOwnerRepo(ownerRepo)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("token रिकामा आहे")
	}
	if err := verifyRepo(token, owner, repo); err != nil {
		return nil, err
	}
	existing, _ := db.ListGHWorkspaces()
	for _, w := range existing {
		if strings.EqualFold(w.Owner, owner) && strings.EqualFold(w.Repo, repo) {
			return nil, fmt.Errorf("%s/%s आधीच add आहे", owner, repo)
		}
	}
	enc, err := encryptToken(token)
	if err != nil {
		return nil, err
	}
	if label = strings.TrimSpace(label); label == "" {
		label = owner
	}
	if eventType = strings.TrimSpace(eventType); eventType == "" {
		eventType = defaultEvent
	}
	w := &db.GHWorkspace{
		ID: uuid.NewString()[:8], Label: label, Owner: owner, Repo: repo, EventType: eventType,
		TokenEnc: enc, Enabled: true, CreatedAt: time.Now(),
	}
	if err := db.InsertGHWorkspace(w); err != nil {
		return nil, err
	}
	return w, nil
}

func SetEnabled(id string, enabled bool) error {
	set := map[string]any{"enabled": enabled}
	if enabled { // पुन्हा "Use" दिलं = error clear
		set["blocked_until"], set["blocked_reason"], set["fail_streak"] = time.Time{}, "", 0
	}
	opMu.Lock()
	err := db.SetGHWorkspaceConfig(id, set)
	opMu.Unlock()
	if err != nil {
		return err
	}
	if !enabled {
		_ = StopNow(id)
	}
	return nil
}

// SetDailyLimit — workspace चा स्वतःचा daily timer (मिनिटांत). 0 = global default.
func SetDailyLimit(id string, minutes int64) error {
	if minutes != 0 && (minutes < 30 || minutes > 1440) {
		return fmt.Errorf("daily limit 30 ते 1440 मिनिटे (0.5–24 तास) हवं, किंवा 0 = default")
	}
	return db.SetGHWorkspaceConfig(id, map[string]any{"daily_limit_min": minutes})
}

func DeleteWorkspace(id string) error {
	_ = StopNow(id)
	return db.DeleteGHWorkspace(id)
}

func load(id string) (*db.GHWorkspace, string, error) {
	ws, err := db.GetGHWorkspace(id)
	if err != nil {
		return nil, "", err
	}
	if ws == nil {
		return nil, "", fmt.Errorf("workspace सापडला नाही")
	}
	tok, err := decryptToken(ws.TokenEnc)
	if err != nil {
		return nil, "", fmt.Errorf("token decrypt failed — workspace पुन्हा add करा")
	}
	return ws, tok, nil
}

// StartNow — manual start: error-block clear, parallel पर्यंत runs सुरू (cooldown/pending ignore).
func StartNow(id string) error {
	opMu.Lock()
	defer opMu.Unlock()
	st := LoadSettings()
	ws, tok, err := load(id)
	if err != nil {
		return err
	}
	now := time.Now()
	if ws.UsageDay != istDay(now) {
		ws.UsageDay, ws.UsedSeconds = istDay(now), 0
	}
	if st.dailyFor(ws)-time.Duration(ws.UsedSeconds)*time.Second < minUsefulWindow {
		return fmt.Errorf("आजचा daily limit (%d min) पूर्ण झालाय", int(st.dailyFor(ws).Minutes()))
	}
	clearBlock(ws)
	runs, err := listRuns(tok, ws.Owner, ws.Repo)
	if err != nil {
		handleErr(ws, err, now)
		_ = db.SaveGHWorkspaceRuntime(ws)
		return err
	}
	for _, r := range runs {
		if isActive(r.Status) {
			_ = db.SaveGHWorkspaceRuntime(ws)
			return fmt.Errorf("run #%d आधीच चालू आहे (एका workspace वर एकच)", r.ID)
		}
	}
	if err := dispatch(tok, ws.Owner, ws.Repo, eventOf(ws)); err != nil {
		handleErr(ws, err, now)
		_ = db.SaveGHWorkspaceRuntime(ws)
		return err
	}
	ws.LastDispatchAt, ws.CooldownUntil = now, time.Time{}
	ws.LastEvent, ws.LastError = "🚀 manual start", ""
	return db.SaveGHWorkspaceRuntime(ws)
}

// StopNow — manual stop: सगळे चालू runs cancel + restart-wait इतका cooldown.
func StopNow(id string) error {
	opMu.Lock()
	defer opMu.Unlock()
	st := LoadSettings()
	ws, tok, err := load(id)
	if err != nil {
		return err
	}
	runs, err := listRuns(tok, ws.Owner, ws.Repo)
	if err != nil {
		return err
	}
	n := 0
	for _, r := range runs {
		if isActive(r.Status) {
			if err := cancelRun(tok, ws.Owner, ws.Repo, r.ID, false); err != nil {
				return err
			}
			n++
		}
	}
	now := time.Now()
	for _, rs := range ws.Runs { // tick बंद असला तरी वापरलेले मिनिटे हरवू नयेत
		if !rs.Start.IsZero() {
			ws.UsedSeconds += int64(now.Sub(rs.Start).Seconds())
		}
	}
	ws.Runs = nil
	ws.CooldownUntil = now.Add(st.wait())
	ws.LastEvent = fmt.Sprintf("🛑 manual stop (%d run)", n)
	return db.SaveGHWorkspaceRuntime(ws)
}

func StartAll() (started int, errs []string) {
	list, err := db.ListGHWorkspaces()
	if err != nil {
		return 0, []string{err.Error()}
	}
	for _, w := range list {
		if !w.Enabled {
			continue
		}
		if err := StartNow(w.ID); err != nil {
			errs = append(errs, w.Label+": "+err.Error())
		} else {
			started++
		}
	}
	return
}

func StopAll() (stopped int, errs []string) {
	list, err := db.ListGHWorkspaces()
	if err != nil {
		return 0, []string{err.Error()}
	}
	for _, w := range list {
		if err := StopNow(w.ID); err != nil {
			errs = append(errs, w.Label+": "+err.Error())
		} else {
			stopped++
		}
	}
	return
}

// ── Status snapshot (GitHub ला call नाही — फक्त DB state) ───────────────────

type WorkspaceStatus struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Repo            string `json:"repo"`
	Enabled         bool   `json:"enabled"`
	State           string `json:"state"` // running | queued | blocked | cooldown | limit | disabled | ready
	Runs            int    `json:"runs"`
	DailyLimitMin   int64  `json:"daily_limit_min"` // effective
	CustomDaily     bool   `json:"custom_daily"`
	UsedMin         int64  `json:"used_min"`
	RunElapsedMin   int64  `json:"run_elapsed_min"`
	CooldownLeftMin int64  `json:"cooldown_left_min"`
	LastEvent       string `json:"last_event"`
	LastError       string `json:"last_error"`
	BlockedReason   string `json:"blocked_reason"`
}

type Snapshot struct {
	Settings   Settings          `json:"settings"`
	Pending    int               `json:"pending"`
	Processing int               `json:"processing"`
	Workspaces []WorkspaceStatus `json:"workspaces"`
}

func GetSnapshot() (*Snapshot, error) {
	list, err := db.ListGHWorkspaces()
	if err != nil {
		return nil, err
	}
	st := LoadSettings()
	now := time.Now()
	snap := &Snapshot{Settings: st, Pending: wmqueue.Default.PendingCount(), Processing: wmqueue.Default.ProcessingCount(),
		Workspaces: []WorkspaceStatus{}}
	for _, w := range list {
		used := time.Duration(0)
		if w.UsageDay == istDay(now) {
			used = time.Duration(w.UsedSeconds) * time.Second
		}
		s := WorkspaceStatus{ID: w.ID, Label: w.Label, Repo: w.Owner + "/" + w.Repo, Enabled: w.Enabled,
			LastEvent: w.LastEvent, LastError: w.LastError, Runs: len(w.Runs),
			DailyLimitMin: int64(st.dailyFor(&w).Minutes()), CustomDaily: w.DailyLimitMin > 0}
		started := 0
		var longest time.Duration
		for _, rs := range w.Runs {
			if rs.Start.IsZero() {
				continue
			}
			started++
			el := now.Sub(rs.Start)
			used += el
			if el > longest {
				longest = el
			}
		}
		s.RunElapsedMin = int64(longest.Minutes())
		switch {
		case started > 0:
			s.State = "running"
		case len(w.Runs) > 0:
			s.State = "queued"
		case isBlocked(&w, now):
			s.State, s.BlockedReason = "blocked", w.BlockedReason
		case now.Before(w.CooldownUntil):
			s.State = "cooldown"
			s.CooldownLeftMin = int64(w.CooldownUntil.Sub(now).Minutes()) + 1
		case st.dailyFor(&w)-used < minUsefulWindow:
			s.State = "limit"
		case !w.Enabled:
			s.State = "disabled"
		default:
			s.State = "ready"
		}
		s.UsedMin = int64(used.Minutes())
		snap.Workspaces = append(snap.Workspaces, s)
	}
	return snap, nil
}
