package main

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/cache"
	"EverythingSuckz/fsb/internal/routes"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/appmanager"
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
	fmt.Println("TRACE: runApp started")
	// initialize logger early so config loading is logged to file
	utils.InitLogger(false)
	config.Load(utils.Logger, cmd)
	// reinitialize with correct dev mode
	utils.InitLogger(config.ValueOf.Dev)
	log := utils.Logger
	mainLogger := log.Named("Main")
	
	fmt.Println("TRACE: Initializing router")
	router := getRouter(log)

	fmt.Println("TRACE: Starting Telegram Bot Client")
	mainBot, err := bot.StartClient(log)
	if err != nil {
		fmt.Println("CRITICAL BOT START ERROR:", err)
		log.Sugar().Fatalf("Failed to start main bot: %v", err)
	}
	
	fmt.Println("TRACE: Telegram Bot Client started successfully")
	cache.InitCache(log)
	
	fmt.Println("TRACE: Starting workers")
	workers, err := bot.StartWorkers(log)
	if err != nil {
		fmt.Println("CRITICAL WORKERS ERROR:", err)
		log.Sugar().Fatalf("Failed to start workers: %v", err)
	}
	
	fmt.Println("TRACE: Adding default client to workers")
	workers.AddDefaultClient(mainBot, mainBot.Self)
	
	fmt.Println("TRACE: Starting UserBot")
	bot.StartUserBot(log)
	
	fmt.Println("TRACE: Initializing Firebase")
	appmanager.InitFirebase(log)
	
	fmt.Println("TRACE: Starting Cleanup Worker")
	go appmanager.StartCleanupWorker(log)

	fmt.Println("TRACE: Running HTTP Router")
	mainLogger.Info("Server started", zap.Int("port", config.ValueOf.Port))
	err = router.Run(fmt.Sprintf(":%d", config.ValueOf.Port))
	if err != nil {
		fmt.Println("CRITICAL ROUTER RUN ERROR:", err)
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
	router.GET("/", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, types.RootResponse{
			Message: "Server is running.",
			Ok:      true,
			Uptime:  utils.TimeFormat(uint64(time.Since(startTime).Seconds())),
			Version: versionString,
		})
	})
	routes.Load(log, router)
	return router
}
