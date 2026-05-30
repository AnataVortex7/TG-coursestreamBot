package main

import (
	"context"
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/cache"
	"EverythingSuckz/fsb/internal/commands"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/routes"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/urlmonitor"
	"EverythingSuckz/fsb/internal/utils"
	workerreg "EverythingSuckz/fsb/internal/worker"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/gin-gonic/gin"
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
	cache.InitCache(log)

	// ── Start background janitor (auto-delete + cleanup) ──────────────────────
	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go commands.StartJanitor(janitorCtx, mainBot, log)

	workers, err := bot.StartWorkers(log)
	if err != nil {
		log.Sugar().Fatalf("Failed to start workers: %v", err)
	}
	workers.AddDefaultClient(mainBot, mainBot.Self)
	bot.StartUserBotFromDB(log)
	// Userbot client commands package ला द्या (import cycle टाळायला)
	commands.MainUserbotClient = bot.UserBot.GetClient()

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
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
 	   SkipPaths: []string{"/", "/courses", "/login"},
	}))
	router.Use(gin.ErrorLogger())
	router.Use(globalSecurityHeaders())

	router.LoadHTMLGlob("web/*")

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
