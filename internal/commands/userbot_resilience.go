// internal/commands/userbot_resilience.go
//
// "rpcDoRequest: engine was closed" / "engine forcibly closed: context canceled" fix.
//
// Karan: /userbotdl survatila userbot client ek var capture hoto. Connection tutla / client
// replace zala (reconnect, redeploy, re-login) ki to client MELELA (engine closed) hoto, pan
// job parat parat tyach marlelya client var retry karto (fakt 3 vela, 5 sec gap) ani tyach
// marlelya client la cache madhun parat parat deto — mhanun manual retry pan same error det hota.
//
// Fix: transient connection error olkhto -> client ping karto -> marla asel tar cache madhun
// kadhun navin ghet -> backoff ne jast vela retry.
package commands

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto"
)

// isTransientConnErr — connection/engine level error (retry + client refresh ne theek hou shakto).
func isTransientConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"engine was closed",
		"engine forcibly closed",
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"unexpected eof",
		"i/o timeout",
		"client is closed",
		"connection closed",
		"connection is closed",
		"connection was aborted",
		"connection refused",
		"no route to host",
		"network is unreachable",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// effectiveUserbotKey — GetUserbotClientForUser jya userID cha client cache madhe thevto.
func effectiveUserbotKey(userID int64) int64 {
	if utils.Contains(config.ValueOf.AllowedUsers, userID) {
		if t, ok := AdminImpersonateStore.Load(userID); ok {
			if tid, ok2 := t.(int64); ok2 {
				return tid
			}
		}
	}
	return userID
}

// pingUserbot — client kharokhar jivant aahe ka (15s timeout).
func pingUserbot(ctx context.Context, c *gotgproto.Client) bool {
	if c == nil {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := c.API().HelpGetNearestDC(pctx)
	return err == nil
}

// reviveUserbot — jivant (ping pass hanara) userbot client dete. Marlela cached client asel tar
// cache madhun kadhun (Stop karun) navin banavto. Admin cha live client asel tar to
// bot.UserBot kadun parat ghetla jato (reconnect/replace zalyavar navin client miltoy).
func reviveUserbot(ctx context.Context, senderID int64) (*gotgproto.Client, error) {
	key := effectiveUserbotKey(senderID)
	var lastErr error
	for i := 0; i < 5; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c, err := GetUserbotClientForUser(senderID)
		if err == nil && c != nil {
			if pingUserbot(ctx, c) {
				return c, nil
			}
			// cached client marla aahe — kadhun taka, pudhchya loop madhe navin banel
			dropUserbotClient(key)
			lastErr = errors.New("userbot client respond karat nahi (ping fail)")
		} else {
			lastErr = err
		}
		select {
		case <-time.After(time.Duration(5*(i+1)) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("userbot client revive zala nahi")
	}
	return nil, lastErr
}

// userbotHolder — job chalu astana client badalta yava mhanun (thread-safe) holder.
type userbotHolder struct {
	mu       sync.RWMutex
	c        *gotgproto.Client
	senderID int64
}

func newUserbotHolder(senderID int64, c *gotgproto.Client) *userbotHolder {
	return &userbotHolder{c: c, senderID: senderID}
}

func (h *userbotHolder) Get() *gotgproto.Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.c
}

// Refresh — client jivant nasel tar navin ghet. Err = revive zala nahi (junach client rahato).
func (h *userbotHolder) Refresh(ctx context.Context) error {
	// Ek vela ek goroutine refresh karel; baki thambtil ani navin client vaprtil.
	h.mu.Lock()
	defer h.mu.Unlock()
	if pingUserbot(ctx, h.c) {
		return nil // dusrya goroutine ne aadhich theek kela / client jivant aahe
	}
	c, err := reviveUserbot(ctx, h.senderID)
	if err != nil {
		return err
	}
	h.c = c
	return nil
}
