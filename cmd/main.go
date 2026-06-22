package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
     "log/slog"
	"github.com/gofiber/fiber/v2"
    "github.com/arpitmandhotra/api-integrator/internal/logger"
	// Ensure your database package is imported here!
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/middleware"
	"github.com/arpitmandhotra/api-integrator/internal/service"
)

func main() {


	// ==========================================
	// 1. THE DATABASE LAYER
	// ==========================================
	redisClient := database.NewRedisClient()
	postgresClient := database.NewPostgresClient()

	postgresClient.AutoMigrate(&domain.Merchant{})
	merchantKey := os.Getenv("MERCHANT_API_KEY")
	if merchantKey == "" {
		log.Fatal("Merchant Key's environment variable is not set")
	}
	shopifySecret := os.Getenv("SHOPIFY_API_SECRET")
	if shopifySecret == "" {
		log.Fatal("CRITICAL: SHOPIFY_API_SECRET environment variable is not set")
	}
	postgresClient.FirstOrCreate(&domain.Merchant{
		StoreName: "Arpit's Test Store",
		APIKey:    merchantKey,
	}, domain.Merchant{APIKey: merchantKey})

	log := logger.New()
	slog.SetDefault(log) 

	slog.Info("starting RTO Intelligence API", "port", 3000)
	// ==========================================
	// 2. THE SERVICE & HANDLER LAYER
	// ==========================================
	trustSvc := service.NewRedisTrustService(redisClient, postgresClient)
	trustHandler := handlers.NewTrustHandler(trustSvc)
  adminHandler := handlers.NewAdminHandler(postgresClient)
	webhookHandler := handlers.NewWebhookHandler(trustSvc)

	// ==========================================
	// 3. THE ROUTER & MIDDLEWARE LAYER
	// ==========================================
	app := fiber.New()
	// 1. The CORS Bridge (Must be FIRST)
    // This tells browser: "Yes, we accept traffic from Shopify frontends, and we expect an API key."
    app.Use(cors.New(cors.Config{
        AllowOrigins: "*", // For local testing. For production, change to "https://your-client.myshopify.com"
        AllowHeaders: "Origin, Content-Type, Accept, X-API-Key",
        AllowMethods: "GET, POST, OPTIONS",
    }))

	// --> THE SHIELD WALL <--
	 // 1. Log traffic
app.Use(middleware.RequestLogger(log))
	
	

	// ==========================================
	// 4. THE ROUTES
	// ==========================================

	// DOOR B: Shopify (Uses Cryptography - NO RATE LIMIT NEEDED YET)
	// Shopify handles its own retries beautifully, so we let their webhooks flow freely for now.
	app.Post("/v1/webhooks/shopify/return", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleOrderReturn)

	// DOOR A: Private Enterprise (Uses Database API Keys + Distributed Redis Limiting)
	app.Post("/v1/trust",
		middleware.RequireAPIKey(postgresClient,redisClient), // 1. Check ID & open the backpack
		middleware.RequireRateLimit(redisClient), // 2. Check Upstash ZSET for Sliding Window limit
		trustHandler.HandleTrustScore,            // 3. Run the Core Engine math
	)
// ==========================================
	// DOOR C: Private Admin Backdoor (Uses Master Key)
	// ==========================================
	app.Post("/v1/internal/onboard", adminHandler.OnboardMerchant)
	adminGroup := app.Group("/v1/admin")
    
    // 1. Place YOUR advanced bouncer at the entrance of the hallway
    // We pass your exact variable names: postgresClient and redisClient
    adminGroup.Use(middleware.RequireAPIKey(postgresClient, redisClient)) 
    
    // 2. Add your secure routes behind the bouncer
    // Your existing unblock route
    adminGroup.Post("/unblock", adminHandler.GetRecentBlocks)

    // THE NEW VAULT DOOR: Your CSV Handler
    adminGroup.Post("/import-csv", adminHandler.ImportBadActorsCSV)

    // 3. Public Health Check (Untouched - this code is perfect)
    app.Get("/health", func(c *fiber.Ctx) error {
        // ping Redis
        _, redisErr := redisClient.Ping(c.UserContext()).Result()
        // ping Postgres
        sqlDB, _ := postgresClient.DB()
        postgresErr := sqlDB.PingContext(c.UserContext())

        if redisErr != nil || postgresErr != nil {
            return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
                "status":   "unhealthy",
                "redis":    redisErr == nil,
                "postgres": postgresErr == nil,
            })
        }
        return c.Status(fiber.StatusOK).JSON(fiber.Map{
            "status": "healthy",
        })
    })
	// Admin Route (We will secure this separately in Sprint 4)
	// 5. START UP — with graceful shutdown
	// =========================================

	// Run server in a goroutine so it doesn't block
	// the signal listener below
	// Run server in a goroutine so it doesn't block
	go func() {
		slog.Info("Starting RTO Intelligence API on port 3000...")
		if err := app.Listen(":3000"); err != nil {
			slog.Error("Server failed to start", "error", err)
			os.Exit(1)
		}
	}()

	// Create a channel to receive OS signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// Block here until a signal arrives
	<-quit

	slog.Info("Shutdown signal received. Draining in-flight requests...")

	// Give active requests up to 30 seconds to complete
	if err := app.ShutdownWithTimeout(30 * time.Second); err != nil {
		slog.Error("Forced shutdown after timeout", "error", err)
		os.Exit(1)
	}
}