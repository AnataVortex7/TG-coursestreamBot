// internal/db/db_replace_rules.go
//
// Per-user (admin-wise) caption replace rules. आधी हे फक्त global in-memory map
// होतं (restart वर जायचं, सगळ्या admins साठी एकच). आता प्रत्येक user चे स्वतःचे
// rules MongoDB मध्ये — restart नंतरही टिकतात.
package db

import (
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ReplaceRule — एक replace rule.
//   - Old  : शोधायचा text (fancy fonts / symbols / मराठी / ओळ — काहीही)
//   - New  : replacement (रिकामा = काढून टाका)
//   - Line : true = Old जिथे सापडेल ती पूर्ण ओळ (line) replace होईल
type ReplaceRule struct {
	Old  string `bson:"old"`
	New  string `bson:"new"`
	Line bool   `bson:"line"`
}

type replaceRulesDoc struct {
	UserID int64         `bson:"_id"`
	Rules  []ReplaceRule `bson:"rules"`
}

func colReplaceRules() *mongo.Collection { return Database.Collection("caption_replace_rules") }

var replaceRulesCache = struct {
	sync.Mutex
	m map[int64]replaceCacheEntry
}{m: map[int64]replaceCacheEntry{}}

type replaceCacheEntry struct {
	rules []ReplaceRule
	at    time.Time
}

const replaceRulesTTL = 10 * time.Second

func GetReplaceRules(userID int64) []ReplaceRule {
	replaceRulesCache.Lock()
	if e, ok := replaceRulesCache.m[userID]; ok && time.Since(e.at) < replaceRulesTTL {
		replaceRulesCache.Unlock()
		return e.rules
	}
	replaceRulesCache.Unlock()

	ctx, cancel := Ctx()
	defer cancel()
	var doc replaceRulesDoc
	rules := []ReplaceRule{}
	if err := colReplaceRules().FindOne(ctx, bson.M{"_id": userID}).Decode(&doc); err == nil {
		rules = doc.Rules
	}
	replaceRulesCache.Lock()
	replaceRulesCache.m[userID] = replaceCacheEntry{rules: rules, at: time.Now()}
	replaceRulesCache.Unlock()
	return rules
}

func SaveReplaceRules(userID int64, rules []ReplaceRule) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := colReplaceRules().UpdateOne(ctx,
		bson.M{"_id": userID},
		bson.M{"$set": bson.M{"rules": rules}},
		options.Update().SetUpsert(true),
	)
	replaceRulesCache.Lock()
	replaceRulesCache.m[userID] = replaceCacheEntry{rules: rules, at: time.Now()}
	replaceRulesCache.Unlock()
	return err
}

// ── Global rules (admin ने add केलेले — सगळ्या users साठी लागू) ──────────────
// Admin चे rules _id = 0 या एका global doc मध्ये ठेवतो. आधी प्रत्येक admin चे
// स्वतंत्र rules होते — पहिल्यांदा global doc तयार होताना ते सगळे आपोआप merge होतात.
const GlobalReplaceUserID int64 = 0

var globalReplaceMigrateOnce sync.Once

func migrateLegacyAdminReplaceRules(adminIDs []int64) {
	ctx, cancel := Ctx()
	defer cancel()
	n, err := colReplaceRules().CountDocuments(ctx, bson.M{"_id": GlobalReplaceUserID})
	if err != nil || n > 0 {
		return
	}
	merged := []ReplaceRule{}
	seen := map[string]bool{}
	for _, id := range adminIDs {
		for _, r := range GetReplaceRules(id) {
			k := fmt.Sprintf("%v|%s|%s", r.Line, r.Old, r.New)
			if seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, r)
		}
	}
	if len(merged) > 0 {
		_ = SaveReplaceRules(GlobalReplaceUserID, merged)
	}
}

// GetGlobalReplaceRules — सगळ्यांसाठी लागू होणारे (admin चे) rules.
func GetGlobalReplaceRules(legacyAdminIDs []int64) []ReplaceRule {
	globalReplaceMigrateOnce.Do(func() { migrateLegacyAdminReplaceRules(legacyAdminIDs) })
	return GetReplaceRules(GlobalReplaceUserID)
}
