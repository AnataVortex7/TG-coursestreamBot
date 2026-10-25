package commands

// AutoSync — stall watchdog.
// Problem: connection "half-dead" zala (error yet nahi, data pan yet nahi) tar download/upload kayam
// atakto (run 49 min ek video var thambla hota). Ata pratyek message attempt var watchdog:
// ek thorda vel (default 180s, env AUTOSYNC_STALL_SEC) progress (bytes) navhta => attempt cancel,
// userbot reconnect ani SAME message parat — run thambat nahi.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var errAsStalled = errors.New("stalled: download/upload progress nahi (connection hang)")

func asStallTimeout() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AUTOSYNC_STALL_SEC"))); err == nil && v >= 30 {
		return time.Duration(v) * time.Second
	}
	return 180 * time.Second
}

// asIsConnErr — connection tutla kiva hang zala (reconnect + same message retry karaycha).
func asIsConnErr(err error) bool {
	return isTransientConnErr(err) || errors.Is(err, errAsStalled)
}

// asProgIdle — kiti vel progress nahi (FloodWait grace madhe 0).
func asProgIdle(id string) time.Duration {
	asProgMu.Lock()
	defer asProgMu.Unlock()
	p := asProgMap[id]
	if p == nil {
		return 0
	}
	now := time.Now()
	if now.Before(p.GraceUntil) {
		return 0
	}
	last := p.Touched
	if p.GraceUntil.After(last) {
		last = p.GraceUntil
	}
	if last.IsZero() {
		return 0
	}
	return now.Sub(last)
}

func asProgGrace(id string, d time.Duration) {
	asProgMu.Lock()
	if p := asProgMap[id]; p != nil {
		p.GraceUntil = time.Now().Add(d)
	}
	asProgMu.Unlock()
}

// asGuard — fn (ek message attempt) var watchdog. Stall zala tar errAsStalled return.
func asGuard(ctx context.Context, taskID string, fn func(ctx context.Context) (int, error)) (int, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	asProgUpdate(taskID, func(p *asProgress) {}) // touch — navin attempt chi survat
	var stalled atomic.Bool
	done := make(chan struct{})
	limit := asStallTimeout()
	go func() {
		tk := time.NewTicker(10 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-actx.Done():
				return
			case <-tk.C:
				if asProgIdle(taskID) > limit {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	id, err := fn(actx)
	close(done)
	if stalled.Load() && ctx.Err() == nil {
		return 0, fmt.Errorf("%w (%v)", errAsStalled, err)
	}
	return id, err
}
