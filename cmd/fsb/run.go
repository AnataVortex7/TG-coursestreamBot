package main

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/cache"
	"EverythingSuckz/fsb/internal/commands"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/routes"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/urlmonitor"
	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/wmqueue"
	workerreg "EverythingSuckz/fsb/internal/worker"
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

var runCmd = &cobra.Command{
	Use:                "run",
	Short:              "Run the bot with the given configuration.",
	DisableSuggestions: false,
	Run:                runApp,
}

var startTime time.Time = time.Now()

func runApp(cmd *cobra.Command, args []string) {
	utils.InitLogger(false)
	config.Load(utils.Logger, cmd)
	utils.InitLogger(config.ValueOf.Dev)
	log := utils.Logger
	mainLogger := log.Named("Main")
	mainLogger.Info("Starting server")

	// ── Connect to MongoDB ────────────────────────────────────────────────────
	db.Connect()
	defer db.Disconnect()
	mainLogger.Info("MongoDB connected")

	// ── Start URL Monitor (standalone — no dependency on bot or stream) ───────
	urlmonitor.Init(log)
	defer urlmonitor.StopAll()

	// ── Build router (loads HTML templates) ──────────────────────────────────
	router := getRouter(log)

	// ── Start Telegram bot ────────────────────────────────────────────────────
	mainBot, err := bot.StartClient(log)
	if err != nil {
		log.Sugar().Fatalf("Failed to start main bot: %v", err)
	}
	commands.MainBotClient = mainBot
	auth.SendTelegramOTP = bot.SendOTPToTelegram // OTP-via-telegram wiring (import cycle टाळण्यासाठी DI)
	cache.InitCache(log)

	// ── Start background janitor (auto-delete + cleanup) ──────────────────────
	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go commands.StartJanitor(janitorCtx, mainBot, log)
	go commands.StartExpiryCleanup(janitorCtx, mainBot, log)
	go commands.StartAutoDelivery(janitorCtx, mainBot, log)
	go commands.StartPendingCourseSend(janitorCtx, mainBot, log) // admin panel varun queue kelele course sends (user link zalyavar)
	go db.StartTGIDSweeper(janitorCtx, []int64(config.ValueOf.AllowedUsers), log) // old bot ला users दिसू नयेत

	workers, err := bot.StartWorkers(log)
	if err != nil {
		log.Sugar().Fatalf("Failed to start workers: %v", err)
	}
	workers.AddDefaultClient(mainBot, mainBot.Self)
	go bot.StartUserBotFromDB(log)
	go func() {
		time.Sleep(2 * time.Second)
		bot.NotifyAllowedUsers("🟢 Service restarted successfully!")
	}()
	// Userbot client commands package ला द्या (import cycle टाळायला)
	commands.GetUserbotClient = bot.UserBot.GetClient
	// Course channel admin-sync hook — cmdAddCourse use karto (import cycle टाळायला)
	commands.SyncChannelAdmins = bot.UserBot.AddBotsAsAdminsForChannel
	// Userbot login/logout/status hooks — menu.go (button menu) वापरतो
	// (import cycle टाळायला: commands ← bot आधीच import करतो).
	commands.UserbotStatus = func() (bool, string, string, string, string, int64) {
		info := bot.UserBot.Status()
		return info.Running, info.FirstName, info.LastName, info.Username, info.Phone, info.UserID
	}
	commands.UserbotSendCode = bot.UserBot.SendCode
	commands.UserbotVerifyCode = func(phone, phoneHash, code, password string) (string, string, string, string, string, int64, error) {
		sessionStr, info, err := bot.UserBot.VerifyCode(phone, phoneHash, code, password)
		return sessionStr, info.FirstName, info.LastName, info.Username, info.Phone, info.UserID, err
	}
	commands.UserbotStop = bot.UserBot.Stop
	commands.UserbotStartFromSession = bot.UserBot.StartFromSession
	commands.GetLiveUserbotClient = bot.UserBot.GetClient
	commands.NotifyAdmins = bot.NotifyAllowedUsers
	commands.SendTelegramDMWithMarkup = func(userID int64, text string, markup interface{}) error {
		if m, ok := markup.(tg.ReplyMarkupClass); ok {
			return bot.SendTelegramDMWithMarkup(userID, text, m)
		}
		return bot.SendTelegramDM(userID, text)
	}
	// Watermark worker (course offline-download flow) साठी — download-bot token
	// आणि त्याच bot च्या स्वतःच्या client मधून resolve केलेला access_hash, दोन्ही
	// एकत्र (mismatch टाळायला — access_hash प्रत्येक bot साठी वेगळा असतो).
	commands.GetDownloadBotForChannel = func(ctx context.Context, channelID int64) (string, int64, error) {
		if len(bot.Workers.Bots) == 0 {
			return "", 0, fmt.Errorf("no worker bots configured")
		}
		w := bot.GetNextWorker()
		if w == nil || w.Client == nil {
			return "", 0, fmt.Errorf("no worker available")
		}
		inputChannel, err := utils.GetChannelPeer(ctx, w.Client.API(), w.Client.PeerStorage, channelID)
		if err != nil {
			return "", 0, err
		}
		return w.BotToken, inputChannel.AccessHash, nil
	}
	// Silent pre-download (/predownload) साठी — upload सुद्धा त्याच bot-pool
	// मधून round-robin करतो (access_hash लागत नाही, फक्त token हवा).
	commands.GetNextUploadBotToken = func() (string, error) {
		if len(bot.Workers.Bots) == 0 {
			return "", fmt.Errorf("no worker bots configured")
		}
		w := bot.GetNextWorker()
		if w == nil {
			return "", fmt.Errorf("no worker available")
		}
		return w.BotToken, nil
	}
	// Watermark progress edits — HTTP Bot API ऐवजी आधी already-चालू असलेल्या
	// persistent MTProto connection वापरून बघतो (जास्त reliable — विशेषतः
	// Hugging Face सारख्या hosting वर जिथे नवीन outbound HTTPS connections
	// अधूनमधून वेळ जास्त लागतो/timeout होतो — "context deadline exceeded").
	// bot.ClientForToken ला client न सापडल्यास किंवा edit error आल्यास false
	// return होतो, आणि wmqueue.EditTgMessage आपोआप जुन्या HTTP मार्गावर
	// fallback होतो — त्यामुळे हे पूर्णपणे safe addition आहे.
	wmqueue.MTProtoEdit = func(botToken string, chatID int64, msgID int, text string) bool {
		client := bot.ClientForToken(botToken)
		if client == nil {
			return false
		}
		peer := client.PeerStorage.GetInputPeerById(chatID)
		if peer == nil {
			return false
		}
		editCtx, editCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer editCancel()
		_, err := client.API().MessagesEditMessage(editCtx, &tg.MessagesEditMessageRequest{
			Peer:    peer,
			ID:      msgID,
			Message: text,
		})
		if err != nil {
			// "message is not modified" निरुपद्रवी आहे — तेच text परत पाठवलं
			// गेलं, actual failure नाही. बाकी कुठलाही error असेल तर HTTP
			// fallback ला संधी द्यायला false return करतो.
			if strings.Contains(err.Error(), "MESSAGE_NOT_MODIFIED") {
				return true
			}
			// [FIX] FLOOD_WAIT (rpc 420) आधी फक्त log होऊन HTTP fallback कडे
			// जायचं — पण HTTP fallback सुद्धा त्याच बॉट session साठी अनेकदा
			// अयशस्वी होतो, आणि पुढच्या progress tick ला परत हेच घडायचं —
			// परिणाम: तासनतास (FLOOD_WAIT कधी कधी 50000+ सेकंद देतो) दर काही
			// सेकंदाला निष्फळ MTProto + HTTP दोन्ही प्रयत्न. आता FLOOD_WAIT
			// सेकंद parse करून wmqueue ला कळवतो — तोपर्यंत EditTgMessage हा
			// botToken साठी दोन्ही मार्ग (network ला हातही न लावता) skip करतो.
			if wait, ok := parseFloodWaitSeconds(err.Error()); ok {
				wmqueue.SetEditFloodWait(botToken, time.Now().Add(wait))
				log.Sugar().Warnw("wmqueue.MTProtoEdit: FLOOD_WAIT — pausing edits for this bot",
					"chatID", chatID, "msgID", msgID, "waitSeconds", wait.Seconds())
				return false
			}
			log.Sugar().Warnw("wmqueue.MTProtoEdit failed — falling back to HTTP", "chatID", chatID, "msgID", msgID, "err", err)
			return false
		}
		return true
	}
	// Registration Telegram notification hook (import cycle टाळायला — auth ← bot direct import karu shakat nahi)
	auth.NotifyAdmins = bot.NotifyAllowedUsers
	wmqueue.MTProtoNotify = func(text string) bool {
		bot.NotifyAllowedUsers(text)
		return true
	}
	// Bot cha PeerStorage redeploy nantar empty asto (fsb.session ephemeral
	// container madhe reset hoто) — LOG_CHANNEL members varun ALLOWED_USERS
	// cha access_hash automatic warm karto, manual "ek message pathवा" lागत nahi.
	bot.WarmAdminPeerCacheDelayed(log, 3*time.Second)
	bot.WarmUserPeerCacheDelayed(log, 5*time.Second) // सगळ्या linked users चा peer cache warm करतो
	bot.SetAttractiveBotMenuCommands(config.ValueOf.BotToken) // Telegram chat bar menu buttons register करतो

	// नवीन deploy असेल तरच (versionString बदलला असेल तरच) सर्व admins + linked
	// users ला "bug fixed, services working" announcement — crash-restart वर
	// परत परत spam होऊ नये म्हणून version-gate. Peer cache warm व्हायला वेळ
	// द्यायला 12 सेकंद उशिरा (वरच्या दोन्ही warm-calls नंतर).
	go func() {
		time.Sleep(12 * time.Second)
		announceDeployIfNewVersion(mainBot, log)
	}()

	// ── Init external worker pool (Koyeb workers) ─────────────────────────────
	// workerreg.Init reads WORKER_URLS / WORKER_URL1..N from config,
	// pings each worker, and starts background health checks.
	// Must run AFTER config.Load and bot startup.
	workerreg.Init(log)

	mainLogger.Info("Server started", zap.Int("port", config.ValueOf.Port))
	mainLogger.Info("File Stream Bot", zap.String("version", versionString))
	mainLogger.Sugar().Infof("Server is running at %s", config.ValueOf.Host)
	mainLogger.Sugar().Infof("External workers: %d healthy", workerreg.HealthyCount())

	err = router.Run(fmt.Sprintf(":%d", config.ValueOf.Port))
	if err != nil {
		mainLogger.Sugar().Fatalf("Server failed to start: %v", err)
	}
}

// floodWaitRe — gotd चा rpc error text असा असतो:
// "rpc error code 420: FLOOD_WAIT (56374)" — कंसातला आकडा seconds आहे.
var floodWaitRe = regexp.MustCompile(`FLOOD_WAIT\s*\((\d+)\)`)

// parseFloodWaitSeconds — error text मधून FLOOD_WAIT चा seconds काढतो.
// सापडला नाही तर ok=false (सामान्य error, FLOOD_WAIT नाही).
func parseFloodWaitSeconds(errText string) (time.Duration, bool) {
	m := floodWaitRe.FindStringSubmatch(errText)
	if m == nil {
		return 0, false
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// globalSecurityHeaders — सर्व routes वर security headers लावतो.
// XSS, clickjacking, MIME sniffing, info leak पासून protect करतो.
func globalSecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Clickjacking — page iframe मध्ये embed होणार नाही
		c.Header("X-Frame-Options", "DENY")

		// MIME sniffing — browser Content-Type ignore करणार नाही
		c.Header("X-Content-Type-Options", "nosniff")

		// XSS filter (older browsers साठी)
		c.Header("X-XSS-Protection", "1; mode=block")

		// Referrer — दुसऱ्या site वर URL leak होणार नाही
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		// HSTS — HTTPS वरच येणे force करतो (production only)
		if os.Getenv("DEV") != "true" {
			c.Header("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		// Server info hide करा
		c.Header("Server", "")

		// ── Content Security Policy ───────────────────────────────────────────
		// XSS attacks आणि malicious script injection रोखतो.
		// 'unsafe-inline' हे inline <script> / <style> tags साठी आवश्यक आहे.
		// object-src 'none': Flash/plugins block करतो.
		// base-uri 'self': base tag injection रोखतो.
		c.Header("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src 'self' https://fonts.gstatic.com data:; "+
				"img-src 'self' data: https: blob:; "+
				"media-src 'self' blob:; "+
				"connect-src 'self'; "+
				"frame-src 'none'; "+
				"object-src 'none'; "+
				"base-uri 'self'",
		)

		// Permissions Policy — camera/mic/location access block करतो
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		c.Next()
	}
}

func getRouter(log *zap.Logger) *gin.Engine {
	if config.ValueOf.Dev {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(prefixAwareLogger())
	router.Use(gin.ErrorLogger())
	router.Use(globalSecurityHeaders())

	router.LoadHTMLGlob("web/*.html")
	router.Static("/static", "web/static")

	router.GET("/ping", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, types.RootResponse{
			Message: "Server is running.",
			Ok:      true,
			Uptime:  utils.TimeFormat(uint64(time.Since(startTime).Seconds())),
		})
	})

	router.StaticFile("/1713b039-f83f-40fd-936c-7225cf5ba339.txt", "web/1713b039-f83f-40fd-936c-7225cf5ba339.txt")

	routes.Load(log, router)

	return router
}

// prefixAwareLogger — Gin v1.9 compatible custom logger middleware.
// SkipPaths exact match करतो, पण /api/watch/position/<token> सारखे
// dynamic paths skip करायला prefix check लागतो.
func prefixAwareLogger() gin.HandlerFunc {
	skipExact := map[string]bool{
		"/": true, "/courses": true, "/login": true, "/ping": true,
	}
	skipPrefixes := []string{
		"/api/watch/position/",
		"/api/worker/stream-check",
		"/api/presence/ping",
		"/api/colab/poll",
		"/api/colab/progress",
		"/api/admin/stream/live",
	}
	logger := gin.LoggerWithConfig(gin.LoggerConfig{})
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if skipExact[p] {
			c.Next()
			return
		}
		for _, prefix := range skipPrefixes {
			if strings.HasPrefix(p, prefix) {
				c.Next()
				return
			}
		}
		logger(c)
	}
}

