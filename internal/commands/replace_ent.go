package commands

// applyAdminReplacesEnt — admin (global) replace rules COMPULSORY — formatting/links sambhalun.
// Jithe jithe main server kadun caption/text user-la kiva channel-la jato (auto-delivery ityadi) tithe vapra.
import (
	"strings"

	"github.com/gotd/td/tg"
)

func applyAdminReplacesEnt(text string, ents []tg.MessageEntityClass) (string, []tg.MessageEntityClass) {
	if strings.TrimSpace(text) == "" {
		return text, ents
	}
	full := applyAdminReplaces(text)
	linkFn := func(u string) (string, bool) {
		nu := applyAdminReplaces(u)
		if strings.TrimSpace(nu) == "" {
			return u, true
		}
		return nu, true
	}
	pieceText, pieceEnts := transformTextEntities(text, asCloneEntities(ents), applyAdminReplaces, linkFn)
	if pieceText == full {
		return pieceText, pieceEnts
	}
	if full == text {
		return text, ents
	}
	if re, ok := asRemapEntities(text, full, asCloneEntities(ents)); ok {
		kept := make([]tg.MessageEntityClass, 0, len(re))
		for _, e := range re {
			if tu, isTU := e.(*tg.MessageEntityTextURL); isTU {
				nu, keep := linkFn(tu.URL)
				if !keep {
					continue
				}
				tu.URL = nu
			}
			kept = append(kept, e)
		}
		return full, kept
	}
	return full, nil
}
