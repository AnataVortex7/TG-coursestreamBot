// internal/bot/userbot_login.go
//
// Admin Panel madhe Telegram Phone Login flow.
// SendCode → VerifyCode (OTP) → 2FA (if needed) → session save → userbot start

package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/pkg/qrlogin"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

// ─── Types ────────────────────────────────────────────────────────────────────

// UserBotInfo — frontend la pathavaychi status
type UserBotInfo struct {
	Running   bool   `json:"running"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Phone     string `json:"phone,omitempty"`
	UserID    int64  `json:"user_id,omitempty"`
}

// pendingLogin — SendCode nantarcha temporary state
type pendingLogin struct {
	phone     string
	phoneHash string
	storage   *session.StorageMemory
	tgClient  *telegram.Client
	cancel    context.CancelFunc
}

// ─── Global state ─────────────────────────────────────────────────────────────

var (
	loginMu      sync.Mutex
	activePending *pendingLogin
)

// ─── Status ──────────────────────────────────────────────────────────────────

func (u *UserBotStruct) Status() UserBotInfo {
	if u.client == nil {
		// DB madhe saved info aahe ka (offline saathi)
		doc, _ := db.GetUserBotSessionDoc()
		if doc != nil && doc.Session != "" {
			return UserBotInfo{
				Running:   false,
				FirstName: doc.FirstName,
				LastName:  doc.LastName,
				Username:  doc.Username,
				Phone:     doc.Phone,
				UserID:    doc.UserID,
			}
		}
		return UserBotInfo{Running: false}
	}
	s := u.client.Self
	return UserBotInfo{
		Running:   true,
		FirstName: s.FirstName,
		LastName:  s.LastName,
		Username:  s.Username,
		Phone:     s.Phone,
		UserID:    s.ID,
	}
}

// ─── SendCode ─────────────────────────────────────────────────────────────────

// SendCode — phone number la Telegram OTP pathavto.
// Returns phoneHash jo VerifyCode la द्यायचा आहे.
func (u *UserBotStruct) SendCode(phone string) (string, error) {
	loginMu.Lock()
	// Juna pending cancel kara
	if activePending != nil {
		activePending.cancel()
		activePending = nil
	}
	loginMu.Unlock()

	apiID  := int(config.ValueOf.ApiID)
	apiHash := config.ValueOf.ApiHash

	storage := &session.StorageMemory{}
	ctx, cancel := context.WithCancel(context.Background())

	tgClient := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: storage,
		Device: telegram.DeviceConfig{
			DeviceModel:   "Pyrogram",
			SystemVersion: runtime.GOOS,
			AppVersion:    "2.0",
		},
	})

	readyCh    := make(chan struct{})
	phoneHashCh := make(chan string, 1)
	errCh       := make(chan error, 1)

	go func() {
		err := tgClient.Run(ctx, func(ctx context.Context) error {
			close(readyCh)

			res, err := tgClient.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
			if err != nil {
				errCh <- err
				return nil
			}
			switch r := res.(type) {
			case *tg.AuthSentCode:
				phoneHashCh <- r.PhoneCodeHash
			default:
				errCh <- fmt.Errorf("unexpected auth type: %T", res)
				return nil
			}

			// VerifyCode येईपर्यंत goroutine alive ठेव
			<-ctx.Done()
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	// Ready होण्याची वाट (max 15s)
	select {
	case <-readyCh:
	case <-time.After(15 * time.Second):
		cancel()
		return "", errors.New("Telegram connection timeout — API_ID / API_HASH check kara")
	}

	// phoneHash येण्याची वाट (max 20s)
	select {
	case hash := <-phoneHashCh:
		loginMu.Lock()
		activePending = &pendingLogin{
			phone:     phone,
			phoneHash: hash,
			storage:   storage,
			tgClient:  tgClient,
			cancel:    cancel,
		}
		loginMu.Unlock()
		return hash, nil
	case err := <-errCh:
		cancel()
		return "", err
	case <-time.After(20 * time.Second):
		cancel()
		return "", errors.New("OTP pathavata aala nahi — phone number check kara")
	}
}

// ─── VerifyCode ───────────────────────────────────────────────────────────────

// VerifyCode — OTP + optional 2FA verify karto.
// password = "" if no 2FA.
// Returns: session string, user info, error
func (u *UserBotStruct) VerifyCode(phone, phoneHash, code, password string) (string, UserBotInfo, error) {
	loginMu.Lock()
	p := activePending
	loginMu.Unlock()

	if p == nil {
		return "", UserBotInfo{}, errors.New("Aadhi phone number pathava (SendCode kara)")
	}
	if p.phone != phone {
		return "", UserBotInfo{}, errors.New("Phone number match hot nahi — aadhi SendCode kara")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type result struct {
		sessionStr string
		info       UserBotInfo
		err        error
	}
	resCh := make(chan result, 1)

	go func() {
		// Step 1: OTP SignIn
		_, signInErr := p.tgClient.Auth().SignIn(ctx, phone, phoneHash, code)

		if signInErr != nil {
			if tgerr.Is(signInErr, "SESSION_PASSWORD_NEEDED") {
				// 2FA required
				if password == "" {
					resCh <- result{err: errors.New("SESSION_PASSWORD_NEEDED")}
					return
				}
				// 2FA verify
				if _, err2FA := p.tgClient.Auth().Password(ctx, password); err2FA != nil {
					resCh <- result{err: fmt.Errorf("2FA password chukicha: %v", err2FA)}
					return
				}
				// 2FA success — continue below
			} else if tgerr.Is(signInErr, "PHONE_CODE_INVALID") {
				resCh <- result{err: errors.New("OTP chukicha aahe — dobara enter kara")}
				return
			} else if tgerr.Is(signInErr, "PHONE_CODE_EXPIRED") {
				resCh <- result{err: errors.New("OTP expire zala — nava OTP maga")}
				return
			} else {
				resCh <- result{err: fmt.Errorf("Login error: %v", signInErr)}
				return
			}
		}

		// User info milav
		self, err := p.tgClient.Self(ctx)
		if err != nil {
			resCh <- result{err: fmt.Errorf("User info milali nahi: %v", err)}
			return
		}

		info := UserBotInfo{
			Running:   true,
			FirstName: self.FirstName,
			LastName:  self.LastName,
			Username:  self.Username,
			Phone:     self.Phone,
			UserID:    self.ID,
		}

		// Session encode (Pyrogram format)
		rawSession, err := p.storage.LoadSession(ctx)
		if err != nil {
			resCh <- result{err: fmt.Errorf("Session load error: %v", err)}
			return
		}

		type jsonData struct {
			Version int
			Data    session.Data
		}
		var jd jsonData
		if jsonErr := json.Unmarshal(rawSession, &jd); jsonErr != nil {
			resCh <- result{err: fmt.Errorf("Session parse error: %v", jsonErr)}
			return
		}

		sessionStr, encErr := qrlogin.EncodeToPyrogramSession(&jd.Data, config.ValueOf.ApiID)
		if encErr != nil {
			resCh <- result{err: fmt.Errorf("Session encode error: %v", encErr)}
			return
		}

		resCh <- result{sessionStr: sessionStr, info: info}
	}()

	res := <-resCh

	// Cleanup
	loginMu.Lock()
	if activePending == p {
		p.cancel()
		activePending = nil
	}
	loginMu.Unlock()

	return res.sessionStr, res.info, res.err
}

// ─── StartFromSession ─────────────────────────────────────────────────────────

// StartFromSession — session string vaparun live userbot restart karto.
func (u *UserBotStruct) StartFromSession(sessionStr string) {
	if u.log == nil {
		u.log = zap.NewNop()
	}
	log := u.log.Named("restart")

	go func() {
		client, err := gotgproto.NewClient(
			int(config.ValueOf.ApiID),
			config.ValueOf.ApiHash,
			gotgproto.ClientTypePhone(""),
			&gotgproto.ClientOpts{
				Session:          sessionMaker.PyrogramSession(sessionStr),
				DisableCopyright: true,
			},
		)
		if err != nil {
			log.Error("Userbot restart failed", zap.Error(err))
			return
		}
		u.client = client
		log.Sugar().Infof("✅ Userbot started: @%s (%s %s)",
			client.Self.Username, client.Self.FirstName, client.Self.LastName)
		_ = u.AddBotsAsAdmins()
	}()
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

func (u *UserBotStruct) Stop() {
	u.client = nil
}

// ─── StartUserBotFromDB ───────────────────────────────────────────────────────

// StartUserBotFromDB — startup var MongoDB madhe session aahe ka check karto.
// run.go madhe bot.StartUserBot(log) REPLACE kara ya ne.
func StartUserBotFromDB(l *zap.Logger) {
	log := l.Named("USERBOT")

	sessionStr, err := db.GetUserBotSession()
	if err != nil {
		log.Error("DB session check failed", zap.Error(err))
	}

	if sessionStr != "" {
		log.Info("DB madhe userbot session sapat — start kartoy")
		config.ValueOf.UserSession = sessionStr
		StartUserBot(l)
		return
	}

	// ENV fallback
	if config.ValueOf.UserSession != "" {
		log.Info("DB session nahi — ENV USER_SESSION use kartoy")
		StartUserBot(l)
		return
	}

	log.Warn("⚠️  Userbot session nahi — Admin Panel > TG Login madhe login kara")
}
