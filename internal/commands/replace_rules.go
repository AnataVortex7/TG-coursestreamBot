// internal/commands/replace_rules.go
//
// Per-user caption replace engine.
//
// Matching "loose" असतो — खालील सगळ्या फरकांकडे दुर्लक्ष करतो:
//   - Fancy Unicode fonts:  𝐃𝐎𝐖𝐍𝐋𝐎𝐀𝐃, 𝘿𝙊𝙒𝙉𝙇𝙊𝘼𝘿, ＤＯＷＮＬＯＡＤ, Ⓓⓞⓦⓝⓛⓞⓐⓓ, ᴅᴏᴡɴʟᴏᴀᴅ  → DOWNLOAD
//   - Upper/lower case
//   - कितीही spaces / NBSP / tabs (एक space समजले जातात)
//   - Zero-width / invisible characters
// Symbols (𓆩𓂍𓆪 ★ ✦ …) आणि मराठी/देवनागरी exact match ने जुळतात (matra नीट टिकतात).
// मूळ caption मधला match झालेला भाग (original characters) replace होतो.
package commands

import (
	"fmt"
	"strings"
	"unicode"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"

	"golang.org/x/text/unicode/norm"
)

// ── folding ──────────────────────────────────────────────────────────────────

var smallCaps = map[rune]rune{
	'ᴀ': 'a', 'ʙ': 'b', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜰ': 'f', 'ɢ': 'g', 'ʜ': 'h',
	'ɪ': 'i', 'ᴊ': 'j', 'ᴋ': 'k', 'ʟ': 'l', 'ᴍ': 'm', 'ɴ': 'n', 'ᴏ': 'o', 'ᴘ': 'p',
	'ǫ': 'q', 'ʀ': 'r', 'ꜱ': 's', 'ᴛ': 't', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴡ': 'w', 'ʏ': 'y', 'ᴢ': 'z',
}

// foldRune — एका मूळ rune चे "folded" runes (0 = ignore, 1+ = normalized).
func foldRune(r rune) []rune {
	if unicode.Is(unicode.Cf, r) || (r >= 0xFE00 && r <= 0xFE0F) {
		return nil // zero-width / variation selector
	}
	if unicode.IsSpace(r) || r == 0x00A0 || r == 0x3164 || r == 0x2800 {
		return []rune{' '}
	}
	if m, ok := smallCaps[r]; ok {
		return []rune{m}
	}
	// 🅰..🆉 (negative squared letters) → a..z
	if r >= 0x1F170 && r <= 0x1F189 {
		return []rune{'a' + (r - 0x1F170)}
	}
	n := norm.NFKC.String(string(r))
	out := make([]rune, 0, len(n))
	for _, x := range n {
		if unicode.IsSpace(x) {
			out = append(out, ' ')
		} else {
			out = append(out, unicode.ToLower(x))
		}
	}
	return out
}

type foldedText struct {
	r     []rune // folded runes
	start []int  // folded[i] → मूळ rune index (start)
	end   []int  // folded[i] → मूळ rune index (end, exclusive)
}

func foldText(orig []rune) foldedText {
	f := foldedText{}
	for i, r := range orig {
		for _, x := range foldRune(r) {
			// space runs एकत्र करतो
			if x == ' ' && len(f.r) > 0 && f.r[len(f.r)-1] == ' ' {
				f.end[len(f.end)-1] = i + 1
				continue
			}
			f.r = append(f.r, x)
			f.start = append(f.start, i)
			f.end = append(f.end, i+1)
		}
	}
	return f
}

func foldPattern(s string) []rune {
	f := foldText([]rune(s))
	p := f.r
	for len(p) > 0 && p[0] == ' ' {
		p = p[1:]
	}
	for len(p) > 0 && p[len(p)-1] == ' ' {
		p = p[:len(p)-1]
	}
	return p
}

func indexRunes(hay, needle []rune, from int) int {
	if len(needle) == 0 {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// ── core ─────────────────────────────────────────────────────────────────────

func applyOneRule(text string, rule db.ReplaceRule) string {
	pat := foldPattern(rule.Old)
	if len(pat) == 0 {
		return text
	}
	orig := []rune(text)
	f := foldText(orig)

	type span struct{ s, e int }
	var spans []span
	pos := 0
	for {
		i := indexRunes(f.r, pat, pos)
		if i < 0 {
			break
		}
		j := i + len(pat) - 1
		spans = append(spans, span{f.start[i], f.end[j]})
		pos = i + len(pat)
	}
	if len(spans) == 0 {
		return text
	}

	// Line mode: span पूर्ण ओळीपर्यंत वाढवतो; एकाच ओळीतले spans merge.
	if rule.Line {
		var merged []span
		for _, sp := range spans {
			s, e := sp.s, sp.e
			for s > 0 && orig[s-1] != '\n' {
				s--
			}
			for e < len(orig) && orig[e] != '\n' {
				e++
			}
			if n := len(merged); n > 0 && merged[n-1].e >= s {
				if e > merged[n-1].e {
					merged[n-1].e = e
				}
				continue
			}
			merged = append(merged, span{s, e})
		}
		spans = merged
	}

	var sb strings.Builder
	last := 0
	for _, sp := range spans {
		sb.WriteString(string(orig[last:sp.s]))
		sb.WriteString(rule.New)
		last = sp.e
	}
	sb.WriteString(string(orig[last:]))
	out := sb.String()

	// Replacement रिकामा असेल तर उरलेल्या रिकाम्या ओळी काढून टाकतो.
	if strings.TrimSpace(rule.New) == "" {
		out = dropBlankedLines(text, out)
	}
	return out
}

// dropBlankedLines — before मध्ये रिकाम्या नसलेल्या पण after मध्ये रिकाम्या झालेल्या ओळी काढतो.
func dropBlankedLines(before, after string) string {
	bl := strings.Split(before, "\n")
	al := strings.Split(after, "\n")
	if len(bl) != len(al) {
		return strings.TrimSpace(after) // line count बदललाय — सोपं cleanup
	}
	var keep []string
	for i := range al {
		if strings.TrimSpace(al[i]) == "" && strings.TrimSpace(bl[i]) != "" {
			continue
		}
		keep = append(keep, al[i])
	}
	return strings.Join(keep, "\n")
}

func applyRules(text string, rules []db.ReplaceRule) string {
	for _, r := range rules {
		text = applyOneRule(text, r)
	}
	return text
}

// applyReplacesForUser — त्या user चे rules.
func applyReplacesForUser(userID int64, text string) string {
	return applyRules(text, db.GetReplaceRules(userID))
}

// applyAdminReplaces — user context नसलेल्या ठिकाणांसाठी (student captions, copychannel…)
// सगळ्या ALLOWED_USERS (admins) चे rules क्रमाने लागू करतो.
func applyAdminReplaces(text string) string {
	for _, id := range config.ValueOf.AllowedUsers {
		text = applyRules(text, db.GetReplaceRules(id))
	}
	return text
}

// ── parsing: "[line:] old => new"  (किंवा  old=new) ─────────────────────────

func parseReplaceSpec(spec string) (db.ReplaceRule, error) {
	spec = strings.TrimLeft(spec, " \t\r\n")
	rule := db.ReplaceRule{}
	low := strings.ToLower(spec)
	if strings.HasPrefix(low, "line:") {
		rule.Line = true
		spec = strings.TrimLeft(spec[len("line:"):], " \t")
	}
	var oldS, newS string
	if i := strings.Index(spec, "=>"); i >= 0 {
		oldS, newS = spec[:i], spec[i+2:]
	} else if i := strings.Index(spec, "="); i >= 0 {
		oldS, newS = spec[:i], spec[i+1:]
	} else {
		return rule, fmt.Errorf("'=>' (किंवा '=') असणे आवश्यक आहे")
	}
	rule.Old = strings.TrimSpace(oldS)
	rule.New = strings.TrimSpace(newS)
	if len(foldPattern(rule.Old)) == 0 {
		return rule, fmt.Errorf("old text रिकामा आहे")
	}
	return rule, nil
}

// commandRest — "/cmd@bot  rest of message (multi-line सह)" → "rest of message"
func commandRest(text string) string {
	t := strings.TrimLeft(text, " \t\r\n")
	i := strings.IndexAny(t, " \t\r\n")
	if i < 0 {
		return ""
	}
	return strings.TrimLeft(t[i:], " \t\r\n")
}

func ruleDisplay(r db.ReplaceRule) string {
	n := r.New
	if strings.TrimSpace(n) == "" {
		n = "(काढून टाका)"
	}
	mode := ""
	if r.Line {
		mode = "📄 line: "
	}
	return fmt.Sprintf("%s`%s` → `%s`", mode, r.Old, n)
}
