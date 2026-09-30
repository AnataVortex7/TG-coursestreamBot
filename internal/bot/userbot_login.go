// internal/bot/userbot_login.go
//
// Raw tg.API() वापरतो — auth.Client wrapper नाही.
// PHONE_CODE_EMPTY fix: AuthSignIn request directly construct होतो.

package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

type UserBotInfo struct {
	Running   bool   `json:"running"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Phone     string `json:"phone,omitempty"`
	UserID    int64  `json:"user_id,omitempty"`
}

type verifyRequest struct {
	code     string
	password string
}

type loginResult struct {
	sessionStr string
	info       UserBotInfo
	err        error
	needs2FA   bool
}

type pendingLogin struct {
	phone     string
	phoneHash string
	verifyCh  chan verifyRequest
	resultCh  chan loginResult
	cancel    context.CancelFunc
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

// SendCode — phone ला OTP पाठवतो.
// apiID/apiHash: 0/"" असेल तर global config वापरतो.
// 3 attempts, प्रत्येकी 60s timeout. Goroutine leak fix केलं आहे.
func (u *UserBotStruct) SendCode(phone string, apiID int, apiHash string) (string, error) {
	if apiID == 0 {
		apiID = int(config.ValueOf.ApiID)
	}
	if apiHash == "" {
		apiHash = config.ValueOf.ApiHash
	}

	loginMu.Lock()
	if activePending != nil {
		activePending.cancel()
		activePending = nil
	}
	loginMu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		hash, err := u.sendCodeAttempt(phone, apiID, apiHash)
		if err == nil {
			return hash, nil
		}
		lastErr = err
		if attempt < 3 {
			// exponential backoff — 3s, 6s
			time.Sleep(time.Duration(attempt) * 3 * time.Second)
		}
	}
	return "", fmt.Errorf("%w (3 attempts failed)", lastErr)
}

func (u *UserBotStruct) sendCodeAttempt(phone string, apiID int, apiHash string) (string, error) {
	storage    := &session.StorageMemory{}
	ctx, cancel := context.WithCancel(context.Background())

	tgClient := telegram.NewClient(apiID, apiHash, telegram.Options{
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
			rawAPI := tgClient.API()

			// ── Step 1: OTP pathav (raw API) ──────────────────────────────
			sentCode, err := rawAPI.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
				PhoneNumber: phone,
				APIID:       int(apiID),
				APIHash:     apiHash,
				Settings:    tg.CodeSettings{},
			})
			if err != nil {
				errCh <- fmt.Errorf("OTP pathavata aala nahi: %v", err)
				return nil
			}

			sc, ok := sentCode.(*tg.AuthSentCode)
			if !ok {
				errCh <- fmt.Errorf("Unexpected response: %T", sentCode)
				return nil
			}
			phoneHash := sc.PhoneCodeHash
			phoneHashCh <- phoneHash

			// ── Step 2: VerifyCode cha wait ────────────────────────────────
			var req verifyRequest
			select {
			case req = <-verifyCh:
			case <-ctx.Done():
				return nil
			case <-time.After(10 * time.Minute):
				resultCh <- loginResult{err: errors.New("OTP timeout — 10 min")}
				return nil
			}

			// ── Step 3: SignIn — raw API — PHONE_CODE_EMPTY yenar nahi ────
			signInRes, signInErr := rawAPI.AuthSignIn(ctx, &tg.AuthSignInRequest{
				PhoneNumber:   phone,
				PhoneCodeHash: phoneHash,
				PhoneCode:     req.code, // exactly jo code dila to
			})

			if signInErr != nil {
				if tgerr.Is(signInErr, "SESSION_PASSWORD_NEEDED") {
					if req.password == "" {
						// Frontend la 2FA signal pathav — goroutine alive
						resultCh <- loginResult{needs2FA: true}
						// 2FA password cha wait
						var req2 verifyRequest
						select {
						case req2 = <-verifyCh:
						case <-ctx.Done():
							return nil
						case <-time.After(10 * time.Minute):
							resultCh <- loginResult{err: errors.New("2FA timeout")}
							return nil
						}
						// 2FA verify — auth.Client.Password() use karto (SRP handle karto)
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
					resultCh <- loginResult{err: errors.New("OTP chukicha — Telegram app madhe OTP bagh")}
					return nil
				} else if tgerr.Is(signInErr, "PHONE_CODE_EXPIRED") {
					resultCh <- loginResult{err: errors.New("OTP expire zala — nava OTP maga")}
					return nil
				} else {
					resultCh <- loginResult{err: fmt.Errorf("Login error: %v", signInErr)}
					return nil
				}
			} else {
				_ = signInRes
			}

			// ── Step 4: Self info ──────────────────────────────────────────
			usersRes, err := rawAPI.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
			if err != nil || len(usersRes) == 0 {
				resultCh <- loginResult{err: fmt.Errorf("User info milali nahi: %v", err)}
				return nil
			}
			self, ok2 := usersRes[0].(*tg.User)
			if !ok2 {
				resultCh <- loginResult{err: errors.New("User type cast failed")}
				return nil
			}

			// ── Step 5: Session encode ─────────────────────────────────────
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
			sessionStr, encErr := qrlogin.EncodeToPyrogramSession(&jd.Data, int32(apiID))
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
			verifyCh:  verifyCh,
			resultCh:  resultCh,
			cancel:    cancel,
		}
		loginMu.Unlock()
		return hash, nil
	case err := <-errCh:
		cancel()
		return "", err
	case <-time.After(60 * time.Second):
		cancel()
		return "", errors.New("Telegram connection timeout — API_ID/API_HASH check kara")
	}
}

// ─── VerifyCode ───────────────────────────────────────────────────────────────

func (u *UserBotStruct) VerifyCode(phone, phoneHash, code, password string) (string, UserBotInfo, error) {
	if code != "" {
		re := regexp.MustCompile(`\b(\d{5})\b`)
		if match := re.FindStringSubmatch(code); match != nil {
			code = match[1]
		} else {
			code = regexp.MustCompile(`\D`).ReplaceAllString(code, "")
		}
	}
	loginMu.Lock()
	p := activePending
	loginMu.Unlock()

	if p == nil {
		return "", UserBotInfo{}, errors.New("Aadhi phone number pathava (SendCode kara)")
	}
	if p.phone != phone {
		return "", UserBotInfo{}, errors.New("Phone number match hot nahi — dobara SendCode kara")
	}

	select {
	case p.verifyCh <- verifyRequest{code: code, password: password}:
	case <-time.After(5 * time.Second):
		return "", UserBotInfo{}, errors.New("Internal timeout — page refresh kara")
	}

	select {
	case res := <-p.resultCh:
		if res.needs2FA {
			return "", UserBotInfo{}, errors.New("SESSION_PASSWORD_NEEDED")
		}
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
		log.Info("DB madhe session sapat — start kartoy")
		config.ValueOf.UserSession = sessionStr
		StartUserBot(l)
		return
	}
	if config.ValueOf.UserSession != "" {
		log.Info("ENV USER_SESSION use kartoy")
		StartUserBot(l)
		return
	}
	log.Warn("⚠️  Userbot session nahi — Admin Panel > TG Login madhe login kara")
}

// auth import use karayala (unused import error nako)
var _ = auth.CodeAuthenticatorFunc(nil)
