package commands

// /asfixrules — destination madhe AADHICH copy zalelya sarv messages var admin (global) + task replace rules
// parat lagu karto (text / caption EDIT). Bot destination madhe admin aahe mhanun fast.
//
//   Phase A: map madhe msg id aahe (AutoSync ne copy kela) — SOURCE message varun parat calculate
//            (rules + link rewrite) ani destination message edit (fakt badalla asel tarach).
//   Phase B: map madhe nahi (junya / dusrya path ne alele) — destination msg ID 1..N scan, tyacha text var
//            idempotent rules (X -> X Patil sarkhe rules skip, text vadhnar nahi).
//
// Mode:
//   f (Fast)         — fakt text / caption edit (file name nahi).
//   n (File name)    — text/caption + jyanche file NAME rule mule badalte tyanche file re-upload (download + upload,
//                      Telegram file name edit karu det nahi) ani message chi media replace. Jast vel/data lagto.
// UI: sagle buttons (asfx:*). /asfixrules fakt menu ughadto.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)


var (
	bfMu     sync.Mutex
	bfCancel context.CancelFunc // /asfixstop
)

type bfStats struct {
	mu       sync.Mutex
	task     string
	taskIdx  int
	taskCnt  int
	phase    string
	done     int
	total    int
	changed  int
	same     int
	deleted  int
	failed   int
	long     int
	perm     int
	fallback int
	note     string
	errs     []string
	started  time.Time
	pstart   time.Time
	mode     string // "f" fast, "n" file name sahit
	renTotal int
	renDone  int
	renFail  int
	renBytes int64
	touched  time.Time // watchdog (Phase C)
}

func (s *bfStats) set(f func(*bfStats)) { s.mu.Lock(); f(s); s.mu.Unlock() }
func (s *bfStats) addErr(msg string) {
	s.mu.Lock()
	if len(s.errs) < 5 {
		s.errs = append(s.errs, msg)
	}
	s.mu.Unlock()
}

func (s *bfStats) render(final bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := "🔧 Replace Backfill • 🟢 LIVE"
	if final {
		head = "🔧 Replace Backfill • ✅ DONE"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s\n📦 Task %d/%d: %s\n📍 %s\n", head, s.taskIdx, s.taskCnt, s.task, s.phase))
	if s.total > 0 {
		pct := s.done * 100 / s.total
		if pct > 100 {
			pct = 100
		}
		sb.WriteString(fmt.Sprintf("[%s] %d%%  %d/%d\n", asBar(s.done, s.total, 12), pct, s.done, s.total))
		if !final && s.done > 0 && s.total > s.done && !s.pstart.IsZero() {
			el := time.Since(s.pstart)
			sb.WriteString(fmt.Sprintf("⏱ ETA ~%s\n", asDur(el/time.Duration(s.done)*time.Duration(s.total-s.done))))
		}
	}
	sb.WriteString(fmt.Sprintf("✏️ Badalle: %d | ➖ Same: %d | 🗑 Delete/nahi: %d | ⚠️ Fail: %d\n", s.changed, s.same, s.deleted, s.failed))
	if s.long > 0 || s.perm > 0 || s.fallback > 0 {
		sb.WriteString(fmt.Sprintf("📏 Caption >1024 skip: %d | 🔒 Edit parvangi nahi: %d | ↩️ Source nahi (dest text vaprla): %d\n", s.long, s.perm, s.fallback))
	}
	if s.mode == "n" && (s.renTotal > 0 || s.renDone > 0) {
		sb.WriteString(fmt.Sprintf("📁 File rename: %d/%d (fail %d) | %s\n", s.renDone, s.renTotal, s.renFail, bfHumanSize(s.renBytes)))
	}
	sb.WriteString(fmt.Sprintf("🕒 Elapsed %s\n", asDur(time.Since(s.started))))
	if s.note != "" {
		sb.WriteString("ℹ️ " + s.note + "\n")
	}
	for _, e := range s.errs {
		sb.WriteString("• " + clipRunes(e, 140) + "\n")
	}
	if final && s.mode != "n" {
		sb.WriteString("\nℹ️ Fast mode: file NAME badalle nahi — fakt text/caption. File name pan hava asel tar menu madhe 🐢 'File name sahit' nivda.")
	}
	return sb.String()
}

// ─── helpers ────────────────────────────────────────────────────────────────

func bfBot[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	for n := 0; n < 4; n++ {
		v, err := utils.ExecuteWithFloodWait(ctx, fn)
		if err == nil || !asIsConnErr(err) || ctx.Err() != nil {
			return v, err
		}
		select {
		case <-time.After(time.Duration(n+1) * 3 * time.Second):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, errors.New("bot connection fail (4 vela)")
}

func bfMsgs(res tg.MessagesMessagesClass) map[int]*tg.Message {
	out := map[int]*tg.Message{}
	var list []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesChannelMessages:
		list = r.Messages
	case *tg.MessagesMessagesSlice:
		list = r.Messages
	case *tg.MessagesMessages:
		list = r.Messages
	}
	for _, mc := range list {
		if m, ok := mc.(*tg.Message); ok {
			out[m.ID] = m
		}
	}
	return out
}

func bfIDs(ids []int) []tg.InputMessageClass {
	out := make([]tg.InputMessageClass, 0, len(ids))
	for _, id := range ids {
		out = append(out, &tg.InputMessageID{ID: id})
	}
	return out
}

func bfURLSig(ents []tg.MessageEntityClass) string {
	var u []string
	for _, e := range ents {
		if tu, ok := e.(*tg.MessageEntityTextURL); ok {
			u = append(u, tu.URL)
		}
	}
	sort.Strings(u)
	return strings.Join(u, "\x00")
}

func bfHasMedia(m *tg.Message) bool {
	switch m.Media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return false
	}
	return true
}

type bfCtx struct {
	ctx     context.Context
	t       *db.AutoSyncTask
	dstC    *gotgproto.Client
	dst     *asChat
	st      *bfStats
	permRun int  // sallagpane "edit parvangi nahi"
	abort   bool // fatal (admin right nahi)
	renames []bfRename
}

// apply — ek destination message var navin text/entities lagu kar (badalla asel tarach edit).
func (b *bfCtx) apply(dm *tg.Message, text string, ents []tg.MessageEntityClass) {
	st := b.st
	if strings.TrimSpace(text) == "" || (text == dm.Message && bfURLSig(ents) == bfURLSig(dm.Entities)) {
		st.set(func(s *bfStats) { s.same++ })
		return
	}
	if bfHasMedia(dm) && len([]rune(text)) > 1024 {
		st.set(func(s *bfStats) { s.long++ })
		return
	}
	_, err := bfBot(b.ctx, func() (tg.UpdatesClass, error) {
		return b.dstC.API().MessagesEditMessage(b.ctx, &tg.MessagesEditMessageRequest{
			Peer:        b.dst.Peer,
			ID:          dm.ID,
			Message:     text,
			Entities:    ents,
			ReplyMarkup: dm.ReplyMarkup, // buttons tikvun thev
		})
	})
	up := ""
	if err != nil {
		up = strings.ToUpper(err.Error())
	}
	switch {
	case err == nil:
		b.permRun = 0
		st.set(func(s *bfStats) { s.changed++ })
		select {
		case <-time.After(350 * time.Millisecond):
		case <-b.ctx.Done():
		}
	case strings.Contains(up, "MESSAGE_NOT_MODIFIED"):
		st.set(func(s *bfStats) { s.same++ })
	case strings.Contains(up, "MESSAGE_ID_INVALID") || strings.Contains(up, "MESSAGE_DELETED"):
		st.set(func(s *bfStats) { s.deleted++ })
	case strings.Contains(up, "CHAT_ADMIN_REQUIRED") || strings.Contains(up, "CHAT_WRITE_FORBIDDEN") ||
		strings.Contains(up, "CHANNEL_PRIVATE") || strings.Contains(up, "USER_BOT_INVALID"):
		b.abort = true
		st.addErr("Bot la destination madhe admin/edit right nahi: " + err.Error())
	case strings.Contains(up, "MESSAGE_AUTHOR_REQUIRED") || strings.Contains(up, "MESSAGE_EDIT_TIME_EXPIRED"):
		b.permRun++
		st.set(func(s *bfStats) { s.perm++ })
		if b.permRun >= 8 {
			b.abort = true
			st.addErr("Sallagpane edit parvangi nahi (8) — bot la 'Edit messages of others' right dya.")
		}
	default:
		st.set(func(s *bfStats) { s.failed++ })
		st.addErr(fmt.Sprintf("msg %d: %v", dm.ID, err))
	}
}

func bfHumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/1024)
	}
}

func bfDocAttrName(m *tg.Message) (string, *tg.Document) {
	if m == nil {
		return "", nil
	}
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return "", nil
	}
	d, ok := md.Document.AsNotEmpty()
	if !ok {
		return "", nil
	}
	for _, a := range d.Attributes {
		if fa, ok := a.(*tg.DocumentAttributeFilename); ok && fa.FileName != "" {
			return fa.FileName, d
		}
	}
	return "", d
}

type bfRename struct {
	ID       int
	Size     int64
	Cur, New string
}

// considerRename — file name rule mule badalto ka? (fakt mode "n")
func (b *bfCtx) considerRename(dm *tg.Message, want func(cur string) string) {
	if b.st.mode != "n" {
		return
	}
	cur, d := bfDocAttrName(dm)
	if cur == "" || d == nil {
		return
	}
	nw := want(cur)
	if nw == "" || nw == cur {
		return
	}
	b.renames = append(b.renames, bfRename{ID: dm.ID, Size: d.Size, Cur: cur, New: nw})
}

// renameFiles — Phase C: download (bot) + upload (bot, navin naav) + MessagesEditMessage(media).
func (b *bfCtx) renameFiles() {
	st := b.st
	if len(b.renames) == 0 {
		return
	}
	var total int64
	for _, r := range b.renames {
		total += r.Size
	}
	st.set(func(s *bfStats) {
		s.phase, s.done, s.total, s.pstart = "Phase C — file name rename (download + upload)", 0, len(b.renames), time.Now()
		s.renTotal, s.renDone, s.renFail, s.renBytes = len(b.renames), 0, 0, total
		s.note = fmt.Sprintf("%d files, ~%s — jast vel lagto", len(b.renames), bfHumanSize(total))
	})
	for _, r := range b.renames {
		if b.abort || b.ctx.Err() != nil {
			return
		}
		if r.Size > 2000*1024*1024 {
			st.set(func(s *bfStats) { s.renFail++; s.done++ })
			st.addErr(fmt.Sprintf("msg %d: file 2GB peksha motha — skip", r.ID))
			continue
		}
		err := b.renameOne(r)
		st.set(func(s *bfStats) { s.done++ })
		if err != nil {
			up := strings.ToUpper(err.Error())
			st.set(func(s *bfStats) { s.renFail++ })
			switch {
			case strings.Contains(up, "CHAT_ADMIN_REQUIRED"):
				b.abort = true
				st.addErr("Bot la destination madhe admin/edit right nahi.")
			case strings.Contains(up, "MESSAGE_AUTHOR_REQUIRED"), strings.Contains(up, "MESSAGE_EDIT_TIME_EXPIRED"):
				st.set(func(s *bfStats) { s.perm++ })
			default:
				st.addErr(fmt.Sprintf("msg %d rename: %v", r.ID, err))
			}
		} else {
			st.set(func(s *bfStats) { s.renDone++ })
		}
	}
	st.set(func(s *bfStats) { s.note = "" })
}

func (b *bfCtx) renameOne(r bfRename) error {
	st := b.st
	res, err := bfBot(b.ctx, func() (tg.MessagesMessagesClass, error) {
		return b.dstC.API().ChannelsGetMessages(b.ctx, &tg.ChannelsGetMessagesRequest{Channel: b.dst.Input, ID: bfIDs([]int{r.ID})})
	})
	if err != nil {
		return err
	}
	dm := bfMsgs(res)[r.ID]
	if dm == nil {
		st.set(func(s *bfStats) { s.deleted++ })
		return nil
	}
	md, ok := dm.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil
	}
	d, ok := md.Document.AsNotEmpty()
	if !ok {
		return nil
	}
	f, err := utils.FileFromMedia(md)
	if err != nil {
		return err
	}
	attrs := make([]tg.DocumentAttributeClass, 0, len(d.Attributes))
	for _, a := range d.Attributes {
		if _, isName := a.(*tg.DocumentAttributeFilename); isName {
			attrs = append(attrs, &tg.DocumentAttributeFilename{FileName: r.New})
			continue
		}
		attrs = append(attrs, a)
	}
	f.FileName = r.New

	// per-file watchdog: 180s bytes nahi => cancel (hang)
	fctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(10 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-fctx.Done():
				return
			case <-tk.C:
				if time.Since(time.Unix(0, last.Load())) > 180*time.Second {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	var lastNote atomic.Int64
	note := func(label string, w, tot int64) {
		last.Store(time.Now().UnixNano())
		if time.Since(time.Unix(0, lastNote.Load())) < time.Second {
			return
		}
		lastNote.Store(time.Now().UnixNano())
		pct := int64(0)
		if tot > 0 {
			pct = w * 100 / tot
		}
		st.set(func(s *bfStats) { s.note = fmt.Sprintf("%s %s %d%% (%s)", label, clipRunes(r.New, 36), pct, bfHumanSize(tot)) })
	}
	upFile, tmp, err := downloadAndReupload(fctx, fctx, b.dstC, b.dstC, f,
		func(w, tot int64) { note("⬇️", w, tot) }, func(w, tot int64) { note("⬆️", w, tot) })
	if tmp != "" {
		defer os.Remove(tmp)
	}
	if err != nil {
		if stalled.Load() {
			return fmt.Errorf("stalled (180s progress nahi): %v", err)
		}
		return err
	}
	_, err = bfBot(b.ctx, func() (tg.UpdatesClass, error) {
		return b.dstC.API().MessagesEditMessage(b.ctx, &tg.MessagesEditMessageRequest{
			Peer:        b.dst.Peer,
			ID:          r.ID,
			Media:       &tg.InputMediaUploadedDocument{File: upFile, MimeType: d.MimeType, Attributes: attrs},
			Message:     dm.Message,
			Entities:    dm.Entities,
			ReplyMarkup: dm.ReplyMarkup,
		})
	})
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "MESSAGE_NOT_MODIFIED") {
		return nil
	}
	return err
}

// ─── ek task ────────────────────────────────────────────────────────────────

func bfRunTask(ctx context.Context, t *db.AutoSyncTask, st *bfStats, seenDest map[int64]map[int]bool) {
	tid := t.ID.Hex()
	dstC, err := asDstClient()
	if err != nil {
		st.addErr("Bot client nahi: " + err.Error())
		return
	}
	dst, err := asResolveChat(ctx, dstC, t.DestChat)
	if err != nil {
		st.addErr("Destination bot la sapadla nahi: " + err.Error())
		return
	}
	asUsernames.Store(tid+":dst", dst.Username)

	// Source (userbot) — Phase A sathi. Session nasel tar dest text var fallback.
	var holder *asClientHolder
	var src *asChat
	if uc, uerr := asClientForKey(ctx, t.SessionKey); uerr == nil {
		holder = &asClientHolder{c: uc, key: t.SessionKey}
		if sc, serr := asResolveChat(ctx, uc, t.SourceChat); serr == nil {
			src = sc
			asUsernames.Store(tid+":src", src.Username)
		}
	}
	if src == nil {
		st.set(func(s *bfStats) { s.note = "Userbot/source nahi — Phase A madhe destination text var rules lagtil." })
	}

	b := &bfCtx{ctx: ctx, t: t, dstC: dstC, dst: dst, st: st}
	idem := *t
	idem.IdemOnly = true
	mapped := map[int]bool{}
	if seenDest[t.DestChat] == nil {
		seenDest[t.DestChat] = map[int]bool{}
	}

	// ── Phase A ──
	total := db.AutoSyncMapCount(t.ID)
	st.set(func(s *bfStats) {
		s.phase, s.done, s.total, s.pstart = "Phase A — map madhle messages (source varun)", 0, total, time.Now()
	})
	after, maxMapped := 0, 0
	for !b.abort && ctx.Err() == nil {
		pairs := db.AutoSyncMapPage(t.ID, after, 100)
		if len(pairs) == 0 {
			break
		}
		after = pairs[len(pairs)-1].Dest
		var dids, sids []int
		for _, p := range pairs {
			dids = append(dids, p.Dest)
			sids = append(sids, p.Src)
			mapped[p.Dest] = true
			seenDest[t.DestChat][p.Dest] = true
			if p.Dest > maxMapped {
				maxMapped = p.Dest
			}
		}
		dres, derr := bfBot(ctx, func() (tg.MessagesMessagesClass, error) {
			return dstC.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: dst.Input, ID: bfIDs(dids)})
		})
		if derr != nil {
			st.set(func(s *bfStats) { s.failed += len(pairs); s.done += len(pairs) })
			st.addErr("destination fetch: " + derr.Error())
			continue
		}
		dmsgs := bfMsgs(dres)
		var smsgs map[int]*tg.Message
		if holder != nil && src != nil {
			sres, serr := asRetryConn(ctx, holder, func(c *gotgproto.Client) (tg.MessagesMessagesClass, error) {
				cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
				defer cancel()
				return utils.ExecuteWithFloodWait(cctx, func() (tg.MessagesMessagesClass, error) {
					return c.API().ChannelsGetMessages(cctx, &tg.ChannelsGetMessagesRequest{Channel: src.Input, ID: bfIDs(sids)})
				})
			})
			if serr == nil {
				smsgs = bfMsgs(sres)
			} else if asIsDeadSessErr(serr) {
				st.set(func(s *bfStats) { s.note = "Userbot session expire — dest text var rules lagtil." })
				holder = nil
			}
		}
		for _, p := range pairs {
			if b.abort || ctx.Err() != nil {
				break
			}
			dm := dmsgs[p.Dest]
			if dm == nil {
				st.set(func(s *bfStats) { s.deleted++; s.done++ })
				continue
			}
			var text string
			var ents []tg.MessageEntityClass
			want := func(cur string) string { return asRenameFile(&idem, cur) }
			if sm := smsgs[p.Src]; sm != nil {
				text, ents, _ = asTransformText(t, sm)
				if srcName, _ := bfDocAttrName(sm); srcName != "" {
					want = func(string) string { return asRenameFile(t, srcName) }
				}
			} else {
				text, ents, _ = asTransformText(&idem, dm)
				st.set(func(s *bfStats) { s.fallback++ })
			}
			b.apply(dm, text, ents)
			b.considerRename(dm, want)
			st.set(func(s *bfStats) { s.done++ })
		}
	}

	// ── Phase B ──
	if b.abort || ctx.Err() != nil {
		return
	}
	st.set(func(s *bfStats) {
		s.phase, s.done, s.total, s.pstart = "Phase B — baki messages (destination text var)", 0, maxMapped+300, time.Now()
	})
	emptyRuns := 0
	for id := 1; !b.abort && ctx.Err() == nil; id += 100 {
		var ids []int
		for k := id; k < id+100; k++ {
			if !seenDest[t.DestChat][k] {
				ids = append(ids, k)
			}
		}
		if len(ids) == 0 {
			st.set(func(s *bfStats) { s.done = id + 99 })
			continue
		}
		res, err := bfBot(ctx, func() (tg.MessagesMessagesClass, error) {
			return dstC.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: dst.Input, ID: bfIDs(ids)})
		})
		if err != nil {
			st.addErr("destination scan: " + err.Error())
			st.set(func(s *bfStats) { s.failed++ })
			emptyRuns++
			if emptyRuns >= 5 {
				break
			}
			continue
		}
		msgs := bfMsgs(res)
		if len(msgs) == 0 {
			emptyRuns++
			if id > maxMapped && emptyRuns >= 10 { // 1000 sallagpane rikame IDs
				break // destination cha shevat (gaps/deleted nantar)
			}
		} else {
			emptyRuns = 0
		}
		keys := make([]int, 0, len(msgs))
		for k := range msgs {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			if b.abort || ctx.Err() != nil {
				break
			}
			seenDest[t.DestChat][k] = true
			dm := msgs[k]
			text, ents, _ := asTransformText(&idem, dm)
			b.apply(dm, text, ents)
			b.considerRename(dm, func(cur string) string { return asRenameFile(&idem, cur) })
		}
		st.set(func(s *bfStats) {
			s.done = id + 99
			if s.done > s.total {
				s.total = s.done + 100
			}
		})
	}
	// ── Phase C (fakt "file name sahit" mode) ──
	if st.mode == "n" && !b.abort && ctx.Err() == nil {
		b.renameFiles()
	}
}

// ─── UI (buttons) ───────────────────────────────────────────────────────────

func bfBtn(text, data string) tg.KeyboardButtonClass {
	return &tg.KeyboardButtonCallback{Text: text, Data: []byte(data)}
}

func bfRows(rows ...[]tg.KeyboardButtonClass) tg.ReplyMarkupClass {
	out := make([]tg.KeyboardButtonRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, tg.KeyboardButtonRow{Buttons: r})
	}
	return &tg.ReplyInlineMarkup{Rows: out}
}

func bfRunning() bool {
	bfMu.Lock()
	defer bfMu.Unlock()
	return bfCancel != nil
}

func bfStopMarkup() tg.ReplyMarkupClass {
	return bfRows([]tg.KeyboardButtonClass{bfBtn("🛑 Stop", "asfx:stop")})
}

func bfDoneMarkup() tg.ReplyMarkupClass {
	return bfRows([]tg.KeyboardButtonClass{bfBtn("🔧 Backfill Menu", "asfx:menu"), bfBtn("📋 AutoSync", "cmd:autosync")})
}

// bfScopeScreen — screen 1: konta task?
func bfScopeScreen() (string, tg.ReplyMarkupClass) {
	if bfRunning() {
		return "🔧 Replace Backfill\n\n⏳ Ek backfill chalu ahe.", bfStopMarkup()
	}
	tasks, err := db.ListAutoSyncTasks()
	if err != nil || len(tasks) == 0 {
		return "🔧 Replace Backfill\n\n❌ Ekahi AutoSync task nahi.", bfRows([]tg.KeyboardButtonClass{bfBtn("⬅️ Back", "cmd:autosync")})
	}
	rows := [][]tg.KeyboardButtonClass{{bfBtn("🌐 Sagle tasks", "asfx:s:all")}}
	for _, t := range tasks {
		rows = append(rows, []tg.KeyboardButtonClass{bfBtn(fmt.Sprintf("📥 %s → %s", clipRunes(t.Name, 18), clipRunes(t.DestTitle, 18)), "asfx:s:"+t.ID.Hex())})
	}
	rows = append(rows, []tg.KeyboardButtonClass{bfBtn("⬅️ Back", "cmd:autosync")})
	return "🔧 Replace Backfill\n\nAadhich copy zalelya messages var admin (global) + task replace rules parat lagu kara.\n\n👇 Konta task?", bfRows(rows...)
}

// bfModeScreen — screen 2: Fast ki File name sahit?
func bfModeScreen(scope string) (string, tg.ReplyMarkupClass) {
	label := "Sagle tasks"
	if scope != "all" {
		if oid, err := primitiveObjectID(scope); err == nil {
			if t, _ := db.GetAutoSyncTask(oid); t != nil {
				label = t.Name
			}
		}
	}
	return fmt.Sprintf("🔧 Replace Backfill\n📦 %s\n\nKasa edit karaycha?\n\n⚡ Fast — fakt text / caption (shevat paryant kahi minutes).\n🐢 File name sahit — text/caption + jyanche FILE NAME rule mule badalte te files parat download + upload hotat (jast vel ani data).", label),
		bfRows(
			[]tg.KeyboardButtonClass{bfBtn("⚡ Fast (text/caption)", "asfx:go:"+scope+":f")},
			[]tg.KeyboardButtonClass{bfBtn("🐢 File name sahit (jast vel)", "asfx:c:"+scope)},
			[]tg.KeyboardButtonClass{bfBtn("⬅️ Back", "asfx:menu")},
		)
}

func bfConfirmScreen(scope string) (string, tg.ReplyMarkupClass) {
	return "🐢 File name sahit\n\n⚠️ Jyanche file NAME rule mule badalte tya pratyek file la:\n• download (bot) → navin naavane upload → message chi media replace\n• file motha asel tar khup vel + data lagto (max 2 GB/file)\n• message ID, link, caption, buttons tasech rahtat\n\nPahile text/caption (fast) hoil, shevti files. Suru karaycha?",
		bfRows(
			[]tg.KeyboardButtonClass{bfBtn("✅ Ho, suru kara", "asfx:go:"+scope+":n")},
			[]tg.KeyboardButtonClass{bfBtn("⬅️ Back", "asfx:s:"+scope)},
		)
}

// bfStart — job suru (callback message = progress message).
func bfStart(adminID int64, msgID int, scope, mode string) (string, bool) {
	tasks, err := db.ListAutoSyncTasks()
	if err != nil || len(tasks) == 0 {
		return "❌ Ekahi AutoSync task nahi.", false
	}
	var sel []db.AutoSyncTask
	if scope == "all" {
		sel = tasks
	} else if oid, perr := primitiveObjectID(scope); perr == nil {
		for _, t := range tasks {
			if t.ID == oid {
				sel = append(sel, t)
			}
		}
	}
	if len(sel) == 0 {
		return "❌ Task sapadla nahi.", false
	}
	if MainBotClient == nil {
		return "❌ Bot client ready nahi.", false
	}
	bfMu.Lock()
	if bfCancel != nil {
		bfMu.Unlock()
		return "⏳ Ek backfill adhich chalu ahe.", false
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	bfCancel = cancel
	bfMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Logger.Sugar().Errorw("asfixrules panic", "panic", r)
			}
			cancel()
			bfMu.Lock()
			bfCancel = nil
			bfMu.Unlock()
		}()
		st := &bfStats{started: time.Now(), taskCnt: len(sel), phase: "suru…", mode: mode}
		jobCtx := utils.WithFloodWaitNotify(runCtx, func(sec int) {
			st.set(func(s *bfStats) { s.note = fmt.Sprintf("⏳ Telegram FloodWait %d sec — aapoaap continue", sec) })
		})
		stop := make(chan struct{})
		var rwg sync.WaitGroup
		rwg.Add(1)
		go func() { // progress reporter (4s)
			defer rwg.Done()
			last := ""
			tk := time.NewTicker(4 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					txt := st.render(false)
					if txt != last {
						if err := asEditProgress(adminID, msgID, txt, bfStopMarkup()); err == nil || isMessageNotModifiedErr(err) {
							last = txt
						}
					}
				}
			}
		}()

		seenDest := map[int64]map[int]bool{}
		for i := range sel {
			if jobCtx.Err() != nil {
				break
			}
			t := sel[i]
			st.set(func(s *bfStats) { s.task, s.taskIdx = t.Name, i+1; s.note = "" })
			if _, running := asRunning.Load(t.ID.Hex()); running {
				st.addErr(fmt.Sprintf("“%s” AutoSync chalu ahe — run sampalyavar parat backfill kara", t.Name))
				continue
			}
			bfRunTask(jobCtx, &t, st, seenDest)
		}
		close(stop)
		rwg.Wait()
		final := st.render(true)
		if jobCtx.Err() != nil {
			final = "🛑 Thambvla / timeout\n" + final
		}
		_ = asEditProgress(adminID, msgID, final, bfDoneMarkup())
	}()
	return "🔧 Replace Backfill suru hot ahe…", true
}

func cmdASFixRules(ctx *ext.Context, u *ext.Update) error {
	text, markup := bfScopeScreen()
	if u.CallbackQuery != nil {
		editMenu(ctx, u, text, markup)
		return dispatcher.EndGroups
	}
	_, _ = ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{Markup: markup})
	return dispatcher.EndGroups
}

func cmdASFixStop(ctx *ext.Context, u *ext.Update) error {
	bfMu.Lock()
	c := bfCancel
	bfMu.Unlock()
	if c == nil {
		reply(ctx, u, "ℹ️ Kontahi backfill chalu nahi.")
		return dispatcher.EndGroups
	}
	c()
	reply(ctx, u, "🛑 Backfill thambvat ahe…")
	return dispatcher.EndGroups
}

// handleASFixCb — asfx:* buttons (admin only).
func handleASFixCb(ctx *ext.Context, u *ext.Update) error {
	if u.CallbackQuery == nil || u.EffectiveUser() == nil {
		return dispatcher.ContinueGroups
	}
	adminID := u.EffectiveUser().GetID()
	if !utils.Contains(config.ValueOf.AllowedUsers, adminID) {
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID})
		return dispatcher.EndGroups
	}
	parts := strings.Split(string(u.CallbackQuery.Data), ":")
	if len(parts) < 2 {
		return dispatcher.EndGroups
	}
	switch parts[1] {
	case "menu":
		text, markup := bfScopeScreen()
		editMenu(ctx, u, text, markup)
	case "s":
		if len(parts) >= 3 {
			text, markup := bfModeScreen(parts[2])
			editMenu(ctx, u, text, markup)
		}
	case "c":
		if len(parts) >= 3 {
			text, markup := bfConfirmScreen(parts[2])
			editMenu(ctx, u, text, markup)
		}
	case "go":
		if len(parts) >= 4 {
			mode := "f"
			if parts[3] == "n" {
				mode = "n"
			}
			msg, ok := bfStart(adminID, u.CallbackQuery.MsgID, parts[2], mode)
			if ok {
				editMenu(ctx, u, msg, bfStopMarkup())
			} else {
				editMenu(ctx, u, msg, bfDoneMarkup())
			}
		}
	case "stop":
		bfMu.Lock()
		c := bfCancel
		bfMu.Unlock()
		if c != nil {
			c()
		}
		_, _ = ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{QueryID: u.CallbackQuery.QueryID, Message: "🛑 Thambvat ahe…"})
	}
	return dispatcher.EndGroups
}
