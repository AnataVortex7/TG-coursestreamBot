package commands

// AutoSync — source channel madhle message links (summary / index messages) destination madhe
// tyach message chya links ne aapoaap replace. User link press kela ki DESTINATION channel madhe
// tya message var jato (source madhe nahi).
//
//   t.me/c/<src>/<msg>, t.me/c/<src>/<topic>/<msg>, t.me/<srcUsername>/<msg>
//     -> https://t.me/c/<dst>/<destMsg>   (kiva t.me/<dstUsername>/<destMsg> jar destination public asel)
//
// Baher che / dusrya channel che links, ani deep links (bots, invite) JASE AAHET TASE.
// Jar linked message ajun destination madhe copy zala nahi (map madhe nahi) tar link tasach rahto ani
// message "link_pending" mark hoto — nantar (next run) edit karun link fix hoto.
import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
)

var asTmeLinkRe = regexp.MustCompile(`(?i)(https?://)?(?:t|telegram)\.me/(c/(\d+)|([A-Za-z][A-Za-z0-9_]{3,}))/(\d+)(?:/(\d+))?`)

var asUsernames sync.Map // "<taskHex>:src" / "<taskHex>:dst" -> username (lowercase, "" = private)

func asRawChannelID(chatID int64) int64 {
	if chatID < 0 {
		return -chatID - 1000000000000
	}
	return chatID
}

func asUname(taskHex, side string) string {
	if v, ok := asUsernames.Load(taskHex + ":" + side); ok {
		return v.(string)
	}
	return ""
}

// asRewriteLinks — text madhle t.me message links source -> destination. unres = ajun resolve na zalele links.
func asRewriteLinks(t *db.AutoSyncTask, s string, unres *int) string {
	if !strings.Contains(strings.ToLower(s), "t.me/") && !strings.Contains(strings.ToLower(s), "telegram.me/") {
		return s
	}
	tid := t.ID.Hex()
	srcRaw := asRawChannelID(t.SourceChat)
	dstRaw := asRawChannelID(t.DestChat)
	srcUser, dstUser := asUname(tid, "src"), asUname(tid, "dst")

	return asTmeLinkRe.ReplaceAllStringFunc(s, func(link string) string {
		sm := asTmeLinkRe.FindStringSubmatch(link)
		if sm == nil {
			return link
		}
		scheme, cPart, cID, user, msgStr := sm[1], sm[2], sm[3], sm[4], sm[5]
		last := msgStr
		if sm[6] != "" { // .../<topic>/<msg>
			last = sm[6]
		}
		// source channel cha link ahe ka?
		isSrc := false
		if strings.HasPrefix(strings.ToLower(cPart), "c/") {
			if id, err := strconv.ParseInt(cID, 10, 64); err == nil && id == srcRaw {
				isSrc = true
			}
		} else if srcUser != "" && strings.EqualFold(user, srcUser) {
			isSrc = true
		}
		if !isSrc {
			return link // baher cha / dusra channel — jasa aahe tasa
		}
		mid, err := strconv.Atoi(last)
		if err != nil {
			return link
		}
		destID, ok := db.AutoSyncMapGet(t.ID, mid)
		if !ok || destID <= 0 {
			if unres != nil {
				*unres++
			}
			return link // target ajun copy zala nahi — nantar fix hoil
		}
		out := fmt.Sprintf("t.me/c/%d/%d", dstRaw, destID)
		if dstUser != "" {
			out = fmt.Sprintf("t.me/%s/%d", dstUser, destID)
		}
		if scheme != "" {
			out = "https://" + out
		}
		return out
	})
}

// asFixPendingLinks — junya messages (summary) jyanche links tevha resolve zale navhte: aata map madhe
// target aale asel tar destination message EDIT karun links fix kar.
func asFixPendingLinks(ctx context.Context, h *asClientHolder, t *db.AutoSyncTask, src, dst *asChat) (fixed int) {
	list := db.AutoSyncLinkPendingList(t.ID, 8, 50)
	if len(list) == 0 {
		return 0
	}
	dstC, err := asDstClient()
	if err != nil {
		return 0
	}
	for _, p := range list {
		if ctx.Err() != nil {
			break
		}
		m, err := asRefetchMsg(ctx, h, src, p.SrcMsg)
		if err != nil {
			if strings.Contains(err.Error(), "sapadla nahi") {
				db.AutoSyncLinkPending(t.ID, p.SrcMsg, false)
			} else {
				db.AutoSyncLinkTry(t.ID, p.SrcMsg)
			}
			continue
		}
		text, ents, unres := asTransformText(t, m)
		if unres > 0 {
			db.AutoSyncLinkTry(t.ID, p.SrcMsg) // ajun target copy zala nahi
			continue
		}
		if strings.TrimSpace(text) == "" || (m.Media != nil && len([]rune(text)) > 1024) {
			db.AutoSyncLinkPending(t.ID, p.SrcMsg, false)
			continue
		}
		_, eerr := utils.ExecuteWithFloodWait(ctx, func() (tg.UpdatesClass, error) {
			return dstC.API().MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
				Peer:     dst.Peer,
				ID:       p.DestMsg,
				Message:  text,
				Entities: ents,
			})
		})
		if eerr != nil && !strings.Contains(strings.ToUpper(eerr.Error()), "MESSAGE_NOT_MODIFIED") {
			db.AutoSyncLinkTry(t.ID, p.SrcMsg)
			continue
		}
		db.AutoSyncLinkPending(t.ID, p.SrcMsg, false)
		fixed++
		select {
		case <-time.After(700 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return fixed
}

var _ *gotgproto.Client // (import)

// ─── username rules → link URLs (t.me/<user>, tg://resolve?domain=<user>) ───────
// Rule "@old=new" fakt text madhle "@old" la match karto. Pan hidden hyperlink / visible URL madhe username
// "t.me/old" kiva "tg://resolve?domain=old" ashya roopat asto (@ nahi) — tya var rule lagat nahi.
// Ata username-rule (Old = @name kiva name) URL forms var pan aapoaap lagto:
//   New = username / @username  → URL madhla username badalto (t.me/old/xyz → t.me/new/xyz)
//   New = full link             → to link vaparto
//   New = saadha shabd ("A")     → hyperlink kadhun takto (old deep link nako), visible URL text madhe shabd.
var (
	asUserTokRe = regexp.MustCompile(`^@?([A-Za-z][A-Za-z0-9_]{3,31})$`)
	asURLUserRe = regexp.MustCompile(`(?i)((?:https?://)?(?:t|telegram)\.me/|(?:https?://)?telegram\.dog/|tg://resolve\?domain=)([A-Za-z][A-Za-z0-9_]{3,31})`)
)

var asTmeReserved = map[string]bool{
	"joinchat": true, "addstickers": true, "addemoji": true, "share": true, "proxy": true, "socks": true,
	"setlanguage": true, "login": true, "invoice": true, "boost": true, "addlist": true, "addtheme": true,
	"confirmphone": true, "contact": true, "bg": true, "iv": true,
}

// asApplyUsernameRules — s madhe URL(s) var username rules. unlink=true => hyperlink kadhun taka.
func asApplyUsernameRules(t *db.AutoSyncTask, s string, isLink bool) (out string, unlink bool) {
	low := strings.ToLower(s)
	if !strings.Contains(low, "t.me/") && !strings.Contains(low, "telegram.me/") &&
		!strings.Contains(low, "telegram.dog/") && !strings.Contains(low, "tg://resolve") {
		return s, false
	}
	rules := asRulesFor(t)
	out = asURLUserRe.ReplaceAllStringFunc(s, func(match string) string {
		sm := asURLUserRe.FindStringSubmatch(match)
		if sm == nil {
			return match
		}
		prefix, user := sm[1], sm[2]
		if asTmeReserved[strings.ToLower(user)] {
			return match
		}
		for _, r := range rules {
			tok := asUserTokRe.FindStringSubmatch(strings.TrimSpace(r.Old))
			if tok == nil || !strings.EqualFold(tok[1], user) {
				continue
			}
			nw := strings.TrimSpace(r.New)
			lnw := strings.ToLower(nw)
			switch {
			case nw == "":
				if isLink {
					unlink = true
				}
				return ""
			case strings.HasPrefix(lnw, "http") || strings.Contains(lnw, "t.me/") || strings.HasPrefix(lnw, "tg://"):
				return nw
			case asUserTokRe.MatchString(nw):
				return prefix + asUserTokRe.FindStringSubmatch(nw)[1]
			default: // saadha shabd — username nahi
				if isLink {
					unlink = true
					return match
				}
				return nw
			}
		}
		return match
	})
	return out, unlink
}

// ─── entity remap (cross-boundary replace) ─────────────────────────────────────
// Rule formatting/link boundary olandto tevha piece-wise replace chalat nahi. Tya veli full text var replace
// karun, old→new text diff (LCS) varun entities cha offsets/length navin text madhe map karto.
// Nahi jamla (text khup motha) tar ok=false.
func asRemapEntities(oldText, newText string, ents []tg.MessageEntityClass) ([]tg.MessageEntityClass, bool) {
	o, n := []rune(oldText), []rune(newText)
	if len(o)*len(n) > 6_000_000 {
		return nil, false
	}
	// common prefix/suffix trim
	pre := 0
	for pre < len(o) && pre < len(n) && o[pre] == n[pre] {
		pre++
	}
	suf := 0
	for suf < len(o)-pre && suf < len(n)-pre && o[len(o)-1-suf] == n[len(n)-1-suf] {
		suf++
	}
	om, nm := o[pre:len(o)-suf], n[pre:len(n)-suf]
	// LCS dp (middle part)
	R, C := len(om), len(nm)
	dp := make([][]uint16, R+1)
	for i := range dp {
		dp[i] = make([]uint16, C+1)
	}
	for i := R - 1; i >= 0; i-- {
		for j := C - 1; j >= 0; j-- {
			if om[i] == nm[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	// startMap[i], endMap[i]: old rune index -> new rune index
	startMap := make([]int, len(o)+1)
	endMap := make([]int, len(o)+1)
	for i := 0; i < pre; i++ {
		startMap[i], endMap[i+1] = i, i+1
	}
	i, j := 0, 0
	blockOldStart, blockNewStart := -1, 0
	flush := func(oldEnd, newEnd int) {
		if blockOldStart < 0 {
			return
		}
		for k := blockOldStart; k < oldEnd; k++ {
			startMap[pre+k] = pre + blockNewStart
			endMap[pre+k+1] = pre + newEnd
		}
		blockOldStart = -1
	}
	for i < R || j < C {
		if i < R && j < C && om[i] == nm[j] {
			flush(i, j)
			startMap[pre+i], endMap[pre+i+1] = pre+j, pre+j+1
			i++
			j++
			continue
		}
		if blockOldStart < 0 {
			blockOldStart, blockNewStart = i, j
		}
		if j < C && (i >= R || dp[i][j+1] >= dp[i+1][j]) {
			j++ // insertion
		} else {
			i++ // deletion
		}
	}
	flush(R, C)
	for k := 0; k < suf; k++ {
		oi := pre + R + k
		ni := pre + C + k
		startMap[oi], endMap[oi+1] = ni, ni+1
	}
	startMap[len(o)] = len(n)
	endMap[0] = 0

	// utf16 <-> rune prefix tables
	pref := func(rs []rune) []int {
		p := make([]int, len(rs)+1)
		for k, r := range rs {
			w := 1
			if r > 0xFFFF {
				w = 2
			}
			p[k+1] = p[k] + w
		}
		return p
	}
	po, pn := pref(o), pref(n)
	toRune := func(p []int, u16 int) int { // utf16 offset -> rune idx
		lo, hi := 0, len(p)-1
		for lo < hi {
			mid := (lo + hi) / 2
			if p[mid] < u16 {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo
	}
	out := make([]tg.MessageEntityClass, 0, len(ents))
	for _, e := range ents {
		off, ln := entFields(e)
		if off == nil || ln == nil {
			out = append(out, e)
			continue
		}
		rs, re := toRune(po, *off), toRune(po, *off+*ln)
		if rs > len(o) || re > len(o) {
			continue
		}
		ns, ne := startMap[rs], endMap[re]
		if ne < ns {
			ne = ns
		}
		*off = pn[ns]
		*ln = pn[ne] - pn[ns]
		if *ln <= 0 {
			continue
		}
		out = append(out, e)
	}
	return out, true
}
