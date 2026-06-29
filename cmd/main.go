package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/logger"
	"github.com/arpitmandhotra/api-integrator/internal/middleware"
	"github.com/arpitmandhotra/api-integrator/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

func main() {
	// ==========================================
	// 1. THE DATABASE LAYER
	// ==========================================
	redisClient := database.NewRedisClient()
	postgresClient := database.NewPostgresClient()

	merchantKey := os.Getenv("MERCHANT_API_KEY")
	if merchantKey == "" {
		log.Fatal("Merchant Key's environment variable is not set")
	}
	
	// Secrets for our Webhook Doors
	shopifySecret := os.Getenv("SHOPIFY_API_SECRET")
	if shopifySecret == "" {
		log.Fatal("CRITICAL: SHOPIFY_API_SECRET environment variable is not set")
	}
	wooSecret := os.Getenv("WOOCOMMERCE_WEBHOOK_SECRET")
	magentoSecret := os.Getenv("MAGENTO_WEBHOOK_SECRET")

	// Validate HASH_PEPPER at startup — prevents silently filling the DB
	// with weak hashes for hours until the first trust check triggers the warning.
	if os.Getenv("HASH_PEPPER") == "" {
		log.Fatal("CRITICAL: HASH_PEPPER environment variable is not set — phone hashes are reversible without a pepper")
	}

	postgresClient.FirstOrCreate(&domain.Merchant{
		StoreName: "Arpit's Test Store",
		APIKey:    merchantKey,
	}, domain.Merchant{APIKey: merchantKey})

	customLog := logger.New()
	slog.SetDefault(customLog)

	slog.Info("starting RTO Intelligence API", "port", 3000)

	// ==========================================
	// 2. THE SERVICE & HANDLER LAYER
	// ==========================================
	trustSvc := service.NewRedisTrustService(redisClient, postgresClient)
	trustHandler := handlers.NewTrustHandler(trustSvc)
	adminHandler := handlers.NewAdminHandler(postgresClient)
	webhookHandler := handlers.NewWebhookHandler(postgresClient)

	// ==========================================
	// 3. THE ROUTER & MIDDLEWARE LAYER
	// ==========================================
	app := fiber.New()
	
	allowedOrigins := os.Getenv("CORS_ALLOWED_ORIGINS")
	if allowedOrigins == "" {
		allowedOrigins = "http://localhost:3000"
	}

	app.Use(cors.New(cors.Config{
		AllowOrigins: allowedOrigins,
		AllowHeaders: "Origin, Content-Type, Accept, X-API-Key",
		AllowMethods: "GET, POST, OPTIONS",
	}))

	app.Use(middleware.RequestLogger(customLog))

	// ==========================================
	// 4. THE ROUTES
	// ==========================================

	// DOOR B: The Omni-Channel Webhook Listeners
	webhookGroup := app.Group("/v1/webhooks")
	
	// Shopify is our primary driver; it MUST have a secret.
	webhookGroup.Post("/shopify", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopify)
	webhookGroup.Post("/shopify/review", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleProductReview)

	// FIX 2: Conditional route registration. 
	// WooCommerce and Magento endpoints only exist if their secrets are configured.
	if wooSecret != "" {
		webhookGroup.Post("/woocommerce", middleware.RequireWooCommerceHMAC(wooSecret), webhookHandler.HandleWooCommerce)
		slog.Info("WooCommerce webhook route active")
	}

	if magentoSecret != "" {
		webhookGroup.Post("/magento", middleware.RequireMagentoAuth(magentoSecret), webhookHandler.HandleMagento)
		slog.Info("Magento webhook route active")
	}
	// DOOR A: Private Enterprise
	app.Post("/v1/trust",
		middleware.RequireAPIKey(postgresClient, redisClient),
		middleware.RequireRateLimit(redisClient),
		trustHandler.HandleTrustScore,
	)

	// DOOR C: Private Admin Backdoor 
	adminGroup := app.Group("/v1/admin")
	adminGroup.Use(middleware.RequireAdminKey())

	adminGroup.Post("/onboard", adminHandler.OnboardMerchant)
	adminGroup.Post("/unblock", adminHandler.GetRecentBlocks)
	adminGroup.Post("/import-csv", adminHandler.ImportBadActorsCSV)

	// ==========================================
	// 5. HEALTH CHECK & START UP
	// ==========================================
	app.Get("/health", func(c *fiber.Ctx) error {
		_, redisErr := redisClient.Ping(c.UserContext()).Result()

		var postgresErr error
		sqlDB, dbErr := postgresClient.DB()
		if dbErr != nil {
			postgresErr = dbErr
		} else {
			postgresErr = sqlDB.PingContext(c.UserContext())
		}

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

	// Run server in a goroutine so it doesn't block the signal listener
	go func() {
		slog.Info("Starting RTO Intelligence API on port 3000...")
		if err := app.Listen(":3000"); err != nil {
			slog.Error("Server failed to start", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful Shutdown sequence
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	<-quit
	slog.Info("Shutdown signal received. Draining in-flight requests...")

	if err := app.ShutdownWithTimeout(30 * time.Second); err != nil {
		slog.Error("Forced shutdown after timeout", "error", err)
	}

	// Close Redis connection pool
	if err := redisClient.Close(); err != nil {
		slog.Error("failed to close Redis connection", "error", err)
	}

	// Close Postgres connection pool
	if sqlDB, err := postgresClient.DB(); err == nil {
		if err := sqlDB.Close(); err != nil {
			slog.Error("failed to close Postgres connection pool", "error", err)
		}
	}

	slog.Info("All connections closed — shutdown complete")
}