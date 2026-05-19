// internal/bot/userbot_login.go
//
// Channel-based login flow — SignIn() Run() च्या आत होतो.
// PHONE_CODE_EMPTY error fix — code channel ne pass hoto.

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

// UserBotInfo — frontend la pathavaychi status
type UserBotInfo struct {
	Running   bool   `json:"running"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Phone     string `json:"phone,omitempty"`
	UserID    int64  `json:"user_id,omitempty"`
}

// verifyRequest — VerifyCode, Run() la channel ne pathavto
type verifyRequest struct {
	code     string
	password string
}

// loginResult — Run() madhe milalela result
type loginResult struct {
	sessionStr string
	info       UserBotInfo
	err        error
	needs2FA   bool
}

// pendingLogin — SendCode nantarcha live state
type pendingLogin struct {
	phone    string
	phoneHash string
	storage  *session.StorageMemory
	verifyCh chan verifyRequest // VerifyCode → Run goroutine
	resultCh chan loginResult   // Run goroutine → VerifyCode
	cancel   context.CancelFunc
}

var (
	loginMu       sync.Mutex
	activePending *pendingLogin
)

// ─── Status ──────────────────────────────────────────────────────────────────

func (u *UserBotStruct) Status() UserBotInfo {
	if u.client == nil {
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

func (u *UserBotStruct) SendCode(phone string) (string, error) {
	loginMu.Lock()
	if activePending != nil {
		activePending.cancel()
		activePending = nil
	}
	loginMu.Unlock()

	storage    := &session.StorageMemory{}
	ctx, cancel := context.WithCancel(context.Background())

	tgClient := telegram.NewClient(int(config.ValueOf.ApiID), config.ValueOf.ApiHash, telegram.Options{
		SessionStorage: storage,
		Device: telegram.DeviceConfig{
			DeviceModel:   "Pyrogram",
			SystemVersion: runtime.GOOS,
			AppVersion:    "2.0",
		},
	})

	phoneHashCh := make(chan string, 1)
	errCh       := make(chan error, 1)
	verifyCh    := make(chan verifyRequest, 1)
	resultCh    := make(chan loginResult, 1)

	go func() {
		runErr := tgClient.Run(ctx, func(ctx context.Context) error {

			// Step 1: OTP pathav
			res, err := tgClient.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
			if err != nil {
				errCh <- fmt.Errorf("SendCode error: %v", err)
				return nil
			}
			var phoneHash string
			switch r := res.(type) {
			case *tg.AuthSentCode:
				phoneHash = r.PhoneCodeHash
			default:
				errCh <- fmt.Errorf("Unexpected auth type: %T", res)
				return nil
			}
			phoneHashCh <- phoneHash

			// Step 2: VerifyCode cha wait — Run() AATACH rahto
			var req verifyRequest
			select {
			case req = <-verifyCh:
			case <-ctx.Done():
				return nil
			case <-time.After(10 * time.Minute):
				resultCh <- loginResult{err: errors.New("OTP timeout — 10 min madhe verify nahi kela")}
				return nil
			}

			// Step 3: SignIn — Run() AATACH hote — PHONE_CODE_EMPTY येणार nahi
			_, signInErr := tgClient.Auth().SignIn(ctx, phone, phoneHash, req.code)
			if signInErr != nil {
				if tgerr.Is(signInErr, "SESSION_PASSWORD_NEEDED") {
					if req.password == "" {
						// Frontend la 2FA signal pathav
						resultCh <- loginResult{needs2FA: true}
						// 2FA password cha wait karo
						var req2 verifyRequest
						select {
						case req2 = <-verifyCh:
						case <-ctx.Done():
							return nil
						case <-time.After(10 * time.Minute):
							resultCh <- loginResult{err: errors.New("2FA timeout")}
							return nil
						}
						if _, err2 := tgClient.Auth().Password(ctx, req2.password); err2 != nil {
							resultCh <- loginResult{err: fmt.Errorf("2FA password chukicha: %v", err2)}
							return nil
						}
					} else {
						if _, err2 := tgClient.Auth().Password(ctx, req.password); err2 != nil {
							resultCh <- loginResult{err: fmt.Errorf("2FA password chukicha: %v", err2)}
							return nil
						}
					}
				} else if tgerr.Is(signInErr, "PHONE_CODE_INVALID") {
					resultCh <- loginResult{err: errors.New("OTP chukicha — dobara check kara")}
					return nil
				} else if tgerr.Is(signInErr, "PHONE_CODE_EXPIRED") {
					resultCh <- loginResult{err: errors.New("OTP expire zala — nava OTP maga")}
					return nil
				} else {
					resultCh <- loginResult{err: fmt.Errorf("Login error: %v", signInErr)}
					return nil
				}
			}

			// Step 4: Self info + session encode
			self, err := tgClient.Self(ctx)
			if err != nil {
				resultCh <- loginResult{err: fmt.Errorf("User info milali nahi: %v", err)}
				return nil
			}

			rawSession, err := storage.LoadSession(ctx)
			if err != nil {
				resultCh <- loginResult{err: fmt.Errorf("Session load error: %v", err)}
				return nil
			}

			type jsonData struct {
				Version int
				Data    session.Data
			}
			var jd jsonData
			if jsonErr := json.Unmarshal(rawSession, &jd); jsonErr != nil {
				resultCh <- loginResult{err: fmt.Errorf("Session parse error: %v", jsonErr)}
				return nil
			}

			sessionStr, encErr := qrlogin.EncodeToPyrogramSession(&jd.Data, config.ValueOf.ApiID)
			if encErr != nil {
				resultCh <- loginResult{err: fmt.Errorf("Session encode error: %v", encErr)}
				return nil
			}

			resultCh <- loginResult{
				sessionStr: sessionStr,
				info: UserBotInfo{
					Running:   true,
					FirstName: self.FirstName,
					LastName:  self.LastName,
					Username:  self.Username,
					Phone:     self.Phone,
					UserID:    self.ID,
				},
			}
			return nil
		})

		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			select {
			case errCh <- runErr:
			default:
			}
		}
	}()

	select {
	case hash := <-phoneHashCh:
		loginMu.Lock()
		activePending = &pendingLogin{
			phone:     phone,
			phoneHash: hash,
			storage:   storage,
			verifyCh:  verifyCh,
			resultCh:  resultCh,
			cancel:    cancel,
		}
		loginMu.Unlock()
		return hash, nil
	case err := <-errCh:
		cancel()
		return "", err
	case <-time.After(20 * time.Second):
		cancel()
		return "", errors.New("Telegram connection timeout — API_ID/API_HASH check kara")
	}
}

// ─── VerifyCode ───────────────────────────────────────────────────────────────

func (u *UserBotStruct) VerifyCode(phone, phoneHash, code, password string) (string, UserBotInfo, error) {
	loginMu.Lock()
	p := activePending
	loginMu.Unlock()

	if p == nil {
		return "", UserBotInfo{}, errors.New("Aadhi phone number pathava (SendCode kara)")
	}
	if p.phone != phone {
		return "", UserBotInfo{}, errors.New("Phone number match hot nahi")
	}

	// Run goroutine la code channel ne pathav
	select {
	case p.verifyCh <- verifyRequest{code: code, password: password}:
	case <-time.After(5 * time.Second):
		return "", UserBotInfo{}, errors.New("Internal timeout — page refresh kara ani dobara try kara")
	}

	// Result cha wait karo
	select {
	case res := <-p.resultCh:
		if res.needs2FA {
			// pending alive asato — 2FA password saathi
			return "", UserBotInfo{}, errors.New("SESSION_PASSWORD_NEEDED")
		}
		// Cleanup
		loginMu.Lock()
		if activePending == p {
			p.cancel()
			activePending = nil
		}
		loginMu.Unlock()
		return res.sessionStr, res.info, res.err
	case <-time.After(30 * time.Second):
		return "", UserBotInfo{}, errors.New("Verify timeout — dobara try kara")
	}
}

// ─── StartFromSession ─────────────────────────────────────────────────────────

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
		log.Sugar().Infof("✅ Userbot started: @%s", client.Self.Username)
		_ = u.AddBotsAsAdmins()
	}()
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

func (u *UserBotStruct) Stop() {
	u.client = nil
}

// ─── StartUserBotFromDB ───────────────────────────────────────────────────────

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

	if config.ValueOf.UserSession != "" {
		log.Info("DB session nahi — ENV USER_SESSION use kartoy")
		StartUserBot(l)
		return
	}

	log.Warn("⚠️  Userbot session nahi — Admin Panel > TG Login madhe login kara")
}
