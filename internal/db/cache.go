// internal/db/cache.go
//
// Static/rarely-changing data (courses, folders, files list, settings) आणि
// per-user session data (access, delivered-cache lookups) साठी एक साधा
// in-memory TTL cache. उद्देश: प्रत्येक button-click ला Mongo ला जावं लागू
// नये — restart झाल्यावर पहिल्या request ला fresh DB मधून भरतो, नंतर बराच
// वेळ त्याच cache मधून जलद उत्तर मिळतं.
//
// साधेपणा/सुरक्षिततेसाठी design choice: admin ने काही बदललं (course/folder/
// file/setting) की त्या संबंधित संपूर्ण bucket ची cache साफ करतो (targeted
// per-item invalidation नाही) — असे बदल क्वचित होतात, त्यामुळे हे स्वस्त आणि
// सुरक्षित आहे. जास्तीत जास्त staleness TTL इतकीच (काही मिनिटं).
package db

import (
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

type ttlCache struct {
	mu   sync.RWMutex
	data map[string]cacheEntry
}

func newTTLCache() *ttlCache {
	return &ttlCache{data: make(map[string]cacheEntry)}
}

func (c *ttlCache) get(key string) (interface{}, bool) {
	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

func (c *ttlCache) set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	c.data[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *ttlCache) delete(key string) {
	c.mu.Lock()
	delete(c.data, key)
	c.mu.Unlock()
}

// clear — संपूर्ण bucket साफ करतो (mutation झाल्यावर).
func (c *ttlCache) clear() {
	c.mu.Lock()
	c.data = make(map[string]cacheEntry)
	c.mu.Unlock()
}

// deleteByPrefix — key ha prefix ne suru hotat te sarv entries kadhto
// (user delete zalyavar tyacha access/delivered cache pusnyasathi).
func (c *ttlCache) deleteByPrefix(prefix string) {
	c.mu.Lock()
	for k := range c.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.data, k)
		}
	}
	c.mu.Unlock()
}

// ── Cache buckets ─────────────────────────────────────────────────────────────
// Static/rarely-changing data — courses, folders-per-course, files-per-course, settings.
var (
	coursesCache  = newTTLCache() // "all" → []Course ; "id:<hex>" → *Course
	foldersCache  = newTTLCache() // "<courseID hex>" → []CourseFolder
	filesCache    = newTTLCache() // "<courseID hex>" → []CachedFile
	settingsCache = newTTLCache() // "<key>" → bool

	// Per-user session data — access checks आणि delivered-cache lookups.
	// Sliding थोडा जास्त वेळ (15 min) ठेवतो कारण एकाच session मध्ये user
	// अनेक folders/lectures बघतो — तितक्या वेळेत access बदलण्याची शक्यता कमी.
	accessCache          = newTTLCache() // "<userID hex>:<courseID hex>" → bool
	deliveredCacheBucket = newTTLCache() // "<userID hex>:<courseID hex>:<messageID>" → *DeliveredCache (nil पण cache होतो — "not found" चाही repeat DB hit टळतो)
)

const (
	staticCacheTTL = 2 * time.Minute
	userCacheTTL   = 15 * time.Minute
)

// invalidateCourseCaches — course/folder/file संबंधित कुठलाही admin बदल
// झाला की call करा (targeted नाही, संपूर्ण bucket साफ — क्वचित होणारी गोष्ट
// असल्यामुळे स्वस्त आणि सुरक्षित).
func invalidateCourseCaches() {
	coursesCache.clear()
	foldersCache.clear()
	filesCache.clear()
}

func invalidateSettingsCache() {
	settingsCache.clear()
}

func invalidateAccessCache(userID, courseID primitive.ObjectID) {
	accessCache.delete(userID.Hex() + ":" + courseID.Hex())
}

// invalidateUserCaches — user permanent delete zalyavar tyacha sarv in-memory
// access / delivered-cache entries pusto (15 min stale access rahu naye).
func invalidateUserCaches(userID primitive.ObjectID) {
	prefix := userID.Hex() + ":"
	accessCache.deleteByPrefix(prefix)
	deliveredCacheBucket.deleteByPrefix(prefix)
}
