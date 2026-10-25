// internal/commands/autosync_links.go
//
// AutoSync sathi 2 goshti:
//
//	A) Summary / index message madhle t.me/c/<source>/<msg> links => destination channel che
//	   NAVIN message links (autosync_map: source msg -> destination msg). User ne link press kela
//	   ki destination madhlya tyach message var jato (source channel var nahi).
//	B) Download prefetch — current message upload hot astana pudhcha media message download hoto
//	   (userbotdl sarkhach pipeline). Download ani upload ekamekanchi vaat baghat nahit.
package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// ─── A) summary links ────────────────────────────────────────────────────────

// asLinkResult — ek message cha link rewrite cha nikal.
type asLinkResult struct {
	Total      int // source channel che kiti links sapadle
	Unresolved int // kiti links cha target ajun destination madhe nahi (copy zala nahi / skip)
}

// asLinkRewriter — source channel che links => destination links.
// Tya task chya DB map madhun (source msg -> destination msg) shodhto.
type asLinkRewriter struct {
	t       *db.AutoSyncTask
	srcRaw  int64
	dstRaw  int64
	mirror  bool
	cache   map[int]int // src msg -> dest msg (0 = nahi)
	res     asLinkResult
	enabled bool
}

func asNewLinkRewriter(t *db.AutoSyncTask, mirror bool) *asLinkRewriter {
	return &asLinkRewriter{
		t:       t,
		srcRaw:  channelRawID(t.SourceChat),
		dstRaw:  channelRawID(t.DestChat),
		mirror:  mirror,
		cache:   map[int]int{},
		enabled: true,
	}
}

func (r *asLinkRewriter) lookup(srcMsg int) int {
	if v, ok := r.cache[srcMsg]; ok {
		return v
	}
	d, ok := db.AutoSyncMapGet(r.t.ID, srcMsg)
	if !ok || d <= 0 {
		d = 0
	}
	r.cache[srcMsg] = d
	return d
}

// rewrite — ek URL. Source channel cha nasel tar jasa aahe tasa. Target copy zala nasel tar
// original link thevto (ani unresolved count vadhavto — run shevti parat fix karto).
func (r *asLinkRewriter) rewrite(u string) string {
	if r == nil || !r.enabled {
		return u
	}
	ch, topic, mid, ok := parseTmeCLink(u)
	if !ok || ch != r.srcRaw {
		return u
	}
	r.res.Total++
	nid := r.lookup(mid)
	if nid <= 0 {
		r.res.Unresolved++
		return u
	}
	if topic > 1 && r.mirror {
		if dt, ok := db.AutoSyncTopicGet(r.t.ID, topic); ok && dt > 0 {
			return fmt.Sprintf("https://t.me/c/%d/%d/%d", r.dstRaw, dt, nid)
		}
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", r.dstRaw, nid)
}

// asUnresolvedSummary — run shevti parat fix karaychya summary msgs.
type asUnresolvedSummary struct {
	SrcMsg  int
	DestMsg int
}

var asUnresolvedMu sync.Mutex
var asUnresolvedMap = map[string][]asUnresolvedSummary{} // task hex ID -> list

func asNoteUnresolved(tid string, srcMsg, destMsg int) {
	if destMsg <= 0 {
		return
	}
	asUnresolvedMu.Lock()
	defer asUnresolvedMu.Unlock()
	for _, u := range asUnresolvedMap[tid] {
		if u.SrcMsg == srcMsg {
			return
		}
	}
	asUnresolvedMap[tid] = append(asUnresolvedMap[tid], asUnresolvedSummary{srcMsg, destMsg})
}

func asTakeUnresolved(tid string) []asUnresolvedSummary {
	asUnresolvedMu.Lock()
	defer asUnresolvedMu.Unlock()
	l := asUnresolvedMap[tid]
	delete(asUnresolvedMap, tid)
	return l
}

// asFixUnresolvedSummaries — run shevti : jya summary msgs madhle links cha target tevha copy navhta,
// tyanna destination madhe EDIT karun links fix karto. returns: fixed, stillUnresolved.
func asFixUnresolvedSummaries(ctx context.Context, h *asClientHolder, t *db.AutoSyncTask, src, dst *asChat, mirror bool) (fixed, still int) {
	list := asTakeUnresolved(t.ID.Hex())
	if len(list) == 0 {
		return 0, 0
	}
	dstC, err := asDstClient()
	if err != nil {
		return 0, len(list)
	}
	for _, u := range list {
		if ctx.Err() != nil {
			still++
			continue
		}
		m, err := asRefetchMsg(ctx, h, src, u.SrcMsg)
		if err != nil {
			still++
			continue
		}
		text, ents, lr := asTransformTextLinks(t, m, mirror)
		if strings.TrimSpace(text) == "" || lr.Total == 0 {
			continue
		}
		if lr.Unresolved >= lr.Total {
			still++ // kahich resolve zala nahi — edit karun kahi upayog nahi
			continue
		}
		_, eErr := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
			return dstC.API().MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
				Peer:      dst.Peer,
				ID:        u.DestMsg,
				Message:   text,
				Entities:  ents,
				NoWebpage: true,
			})
		})
		if eErr != nil && !strings.Contains(strings.ToUpper(eErr.Error()), "MESSAGE_NOT_MODIFIED") {
			if asLog != nil {
				asLog.Warn("AutoSync summary link fix edit failed", zap.Error(eErr), zap.Int("destMsg", u.DestMsg))
			}
			still++
			continue
		}
		if lr.Unresolved > 0 {
			still++
		}
		fixed++
	}
	return fixed, still
}

// ─── B) download prefetch ────────────────────────────────────────────────────

// asPre — ek message cha background download.
type asPre struct {
	msgID  int
	done   chan struct{}
	path   string
	size   int64
	err    error
	cancel context.CancelFunc
}

var asPreMu sync.Mutex
var asPreMap = map[string]*asPre{} // task hex ID -> current prefetch (ekach)

func asPrefetchEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTOSYNC_PREFETCH")))
	return v != "0" && v != "false" && v != "off"
}

// asPrefetchMaxBytes — evdhya mothya file cha prefetch karaycha nahi (disk). Default 2 GB.
func asPrefetchMaxBytes() int64 {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AUTOSYNC_PREFETCH_MAX_MB"))); err == nil && n > 0 {
		return int64(n) * 1024 * 1024
	}
	return 2048 * 1024 * 1024
}

// asDownloadToTemp — file temp madhe parallel download (userbotdl che dlThreads vaparto).
func asDownloadToTemp(ctx context.Context, c *gotgproto.Client, f *types.File, onProgress func(w, tot int64)) (string, error) {
	tmp, err := os.CreateTemp("", "autosync-*.tmp")
	if err != nil {
		return "", fmt.Errorf("temp file create failed: %w", err)
	}
	path := tmp.Name()
	var w io.WriterAt = tmp
	if onProgress != nil {
		w = &progressWriter{w: tmp, total: f.FileSize, onProgress: onProgress}
	}
	_, err = downloader.NewDownloader().Download(c.API(), f.Location).WithThreads(dlThreads).Parallel(ctx, w)
	_ = tmp.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("download failed: %w", err)
	}
	return path, nil
}

// asUploadFromTemp — temp file bot kadun upload (userbotdl che upThreads / part size).
func asUploadFromTemp(ctx context.Context, up *gotgproto.Client, name, path string, onProgress func(w, tot int64)) (tg.InputFileClass, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("temp file stat failed: %w", err)
	}
	rf, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("temp file reopen failed: %w", err)
	}
	defer rf.Close()
	u := uploader.NewUploader(up.API()).WithPartSize(upPartSize).WithThreads(upThreads)
	if onProgress != nil {
		u = u.WithProgress(&uploadProgressAdapter{onProgress: onProgress})
	}
	file, err := u.Upload(ctx, uploader.NewUpload(name, rf, st.Size()))
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	return file, nil
}

// asStartPrefetch — m (document) cha download background madhe suru karto. Aadhicha prefetch asel tar cancel.
func asStartPrefetch(parent context.Context, c *gotgproto.Client, tid string, m *tg.Message) {
	if !asPrefetchEnabled() || c == nil || m == nil {
		return
	}
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return
	}
	d, ok := md.Document.AsNotEmpty()
	if !ok || d.Size <= 0 || d.Size > asPrefetchMaxBytes() {
		return
	}
	f, err := utils.FileFromMedia(md)
	if err != nil {
		return
	}
	asPreMu.Lock()
	if cur := asPreMap[tid]; cur != nil {
		if cur.msgID == m.ID {
			asPreMu.Unlock()
			return
		}
		cur.cancel()
		go asDiscardPre(cur)
	}
	ctx, cancel := context.WithCancel(parent)
	p := &asPre{msgID: m.ID, done: make(chan struct{}), cancel: cancel}
	asPreMap[tid] = p
	asPreMu.Unlock()

	go func() {
		defer close(p.done)
		path, err := asDownloadToTemp(ctx, c, f, nil)
		p.path, p.err = path, err
		if err == nil {
			p.size = d.Size
		}
	}()
}

func asPrefetchActiveFor(tid string, msgID int) bool {
	asPreMu.Lock()
	defer asPreMu.Unlock()
	p := asPreMap[tid]
	return p != nil && p.msgID == msgID
}

func asDiscardPre(p *asPre) {
	<-p.done
	if p.path != "" {
		_ = os.Remove(p.path)
	}
}

// asTakePrefetch — msgID cha prefetch tayar asel tar (path) dyato; nasel / fail asel tar "".
// Prefetch chalu asel tar tyachi vaat baghto (tyat tyachi download progress dakhavli jat nahi).
func asTakePrefetch(ctx context.Context, tid string, msgID int) string {
	asPreMu.Lock()
	p := asPreMap[tid]
	if p == nil || p.msgID != msgID {
		asPreMu.Unlock()
		return ""
	}
	delete(asPreMap, tid)
	asPreMu.Unlock()

	select {
	case <-p.done:
	case <-ctx.Done():
		p.cancel()
		go asDiscardPre(p)
		return ""
	}
	if p.err != nil || p.path == "" {
		return ""
	}
	return p.path
}

// asClearPrefetch — run sampla / thambla : bakiche prefetch cancel + temp delete.
func asClearPrefetch(tid string) {
	asPreMu.Lock()
	p := asPreMap[tid]
	delete(asPreMap, tid)
	asPreMu.Unlock()
	if p != nil {
		p.cancel()
		go asDiscardPre(p)
	}
}

// asNextDocMsg — msgs[from:] madhla pahila navin (aadhi copy na zalela) document message.
func asNextDocMsg(t *db.AutoSyncTask, msgs []*tg.Message, from int) *tg.Message {
	for i := from; i < len(msgs) && i < from+3; i++ { // fakt javalche 3 baghto
		m := msgs[i]
		if _, done := db.AutoSyncMapGet(t.ID, m.ID); done {
			continue
		}
		switch m.Media.(type) {
		case *tg.MessageMediaDocument:
			return m
		case *tg.MessageMediaPhoto:
			continue
		}
	}
	return nil
}

var _ = time.Second
