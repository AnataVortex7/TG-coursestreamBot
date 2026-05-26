// Package presence tracks whether any user is actively on a course page.
// Main server uses this to decide when to put admin-added workers to sleep.
package presence

import (
	"sync"
	"time"
)

var (
	mu         sync.Mutex
	activeUsers = make(map[string]time.Time) // userIDHex → last ping time
	lastActive  time.Time                    // last time anyone was active
)

func init() {
	lastActive = time.Now() // server start = considered active
}

// MarkActive records that this user is currently on a course page.
// Called by POST /api/presence/ping.
func MarkActive(userIDHex string) {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	activeUsers[userIDHex] = now
	lastActive = now
}

// Sweep removes users whose last ping was > 90 seconds ago (they left the page).
// Called periodically by the sleep-check loop in workerreg.
func Sweep() {
	mu.Lock()
	defer mu.Unlock()
	cutoff := time.Now().Add(-90 * time.Second)
	for id, t := range activeUsers {
		if t.Before(cutoff) {
			delete(activeUsers, id)
		}
	}
}

// IsAnyoneActive returns true if at least one user pinged within the last 90s.
func IsAnyoneActive() bool {
	mu.Lock()
	defer mu.Unlock()
	if len(activeUsers) > 0 {
		return true
	}
	return false
}

// LastActiveTime returns the last time any user was seen on a course page.
func LastActiveTime() time.Time {
	mu.Lock()
	defer mu.Unlock()
	return lastActive
}
