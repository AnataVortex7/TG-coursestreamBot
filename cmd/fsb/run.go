package main

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/cache"
	"EverythingSuckz/fsb/internal/commands"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/routes"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	workerreg "EverythingSuckz/fsb/internal/worker"
	"fmt"
	"net/http"
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

	// ── Build router (loads HTML templates) ──────────────────────────────────
	router := getRouter(log)

	// ── Start Telegram bot ────────────────────────────────────────────────────
	mainBot, err := bot.StartClient(log)
	if err != nil {
		log.Sugar().Fatalf("Failed to start main bot: %v", err)
	}
	commands.MainBotClient = mainBot
	cache.InitCache(log)
	workers, err := bot.StartWorkers(log)
	if err != nil {
		log.Sugar().Fatalf("Failed to start workers: %v", err)
	}
	workers.AddDefaultClient(mainBot, mainBot.Self)
	bot.StartUserBot(log)

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

func getRouter(log *zap.Logger) *gin.Engine {
	if config.ValueOf.Dev {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()
	router.Use(gin.ErrorLogger())

	router.LoadHTMLGlob("web/*")

	router.GET("/ping", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, types.RootResponse{
			Message: "Server is running.",
			Ok:      true,
			Uptime:  utils.TimeFormat(uint64(time.Since(startTime).Seconds())),
			Version: versionString,
		})
	})

	// ── Worker status endpoint (optional — useful for monitoring) ─────────────
	router.GET("/workers/status", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"healthy": workerreg.HealthyCount(),
			"workers": workerreg.Status(),
		})
	})

	routes.Load(log, router)

	return router
}
