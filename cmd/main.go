package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/billing"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/logger"
	"github.com/arpitmandhotra/api-integrator/internal/middleware"
	"github.com/arpitmandhotra/api-integrator/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

func main() {
	// ==========================================
	// 1. THE DATABASE LAYER
	// ==========================================
	postgresClient := database.NewPostgresClient()
	redisClient := database.NewRedisClient()

	// Initialize billing engine database and Redis singletons
	billing.DB = postgresClient
	billing.Redis = redisClient

	// Secrets for our Webhook Doors
	shopifySecret := os.Getenv("SHOPIFY_API_SECRET")
	if shopifySecret == "" {
		log.Fatal("CRITICAL: SHOPIFY_API_SECRET environment variable is not set")
	}
	wooSecret := os.Getenv("WOOCOMMERCE_WEBHOOK_SECRET")
	magentoSecret := os.Getenv("MAGENTO_WEBHOOK_SECRET")

	// Validate KOTMAN_GLOBAL_PEPPER at startup
	if os.Getenv("KOTMAN_GLOBAL_PEPPER") == "" {
		log.Fatal("CRITICAL: KOTMAN_GLOBAL_PEPPER environment variable is not set — phone hashes are reversible without a pepper")
	}

	customLog := logger.New()
	slog.SetDefault(customLog)

	slog.Info("starting RTO Intelligence API", "port", 8080)

	// ==========================================
	// 2. THE SERVICE & HANDLER LAYER
	// ==========================================
	trustSvc := service.NewRedisTrustService(redisClient, postgresClient)
	trustHandler := handlers.NewTrustHandler(trustSvc)
	csvSvc := service.NewCSVImportService(postgresClient, redisClient)
	adminHandler := handlers.NewAdminHandler(postgresClient, csvSvc)
	webhookHandler := handlers.NewWebhookHandler(postgresClient, redisClient, shopifySecret, wooSecret, magentoSecret)
	oauthHandler := handlers.NewOAuthHandler(postgresClient, redisClient)
	magentoHandler := handlers.NewMagentoOnboardHandler(postgresClient)
	analyticsHandler := handlers.NewAnalyticsHandler(postgresClient, redisClient)
	onboardingHandler := handlers.NewOnboardingHandler(postgresClient)
	billingHandler := handlers.NewBillingHandler(postgresClient, redisClient)

	// ==========================================
	// 3. THE ROUTER & MIDDLEWARE LAYER
	// ==========================================
	app := fiber.New(fiber.Config{
		BodyLimit: 1 * 1024 * 1024, // 1MB — Shopify webhooks are never legitimately larger
	})
	
	app.Use(recover.New())
	
	allowedOrigins := os.Getenv("CORS_ALLOWED_ORIGINS")
	if allowedOrigins == "" {
		allowedOrigins = "http://localhost:3000"
	}

	app.Use(cors.New(cors.Config{
		AllowOrigins: allowedOrigins,
		AllowHeaders: "Origin, Content-Type, Accept, X-API-Key",
		AllowMethods: "GET, POST, OPTIONS",
	}))

	app.Use(func(c *fiber.Ctx) error {
		// Do not add security headers to webhooks to avoid parsing failures in legacy integrations
		if strings.HasPrefix(c.Path(), "/v1/webhooks") {
			return c.Next()
		}
		c.Set("X-Frame-Options", "DENY")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-XSS-Protection", "1; mode=block")
		c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		c.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		// Safely handle X-Request-ID and type assertion to avoid panics if requestid locals is nil
		reqID := c.Get("X-Request-ID")
		if reqID == "" {
			if lIDVal := c.Locals("requestid"); lIDVal != nil {
				if lIDStr, ok := lIDVal.(string); ok {
					reqID = lIDStr
				}
			}
		}
		if reqID != "" {
			c.Set("X-Request-ID", reqID)
		}
		return c.Next()
	})

	app.Use(middleware.RequestLogger(customLog))

	ipLimiter := middleware.RequireIPRateLimit(redisClient, 20)

	// ==========================================
	// 4. THE ROUTES
	// ==========================================

	// Public Onboarding Routes (Shopify & WooCommerce OAuth)
	app.Post("/v1/merchants/register", ipLimiter, onboardingHandler.RegisterMerchant)
	app.Get("/auth/shopify/install", ipLimiter, oauthHandler.HandleShopifyInstall)
	app.Get("/auth/shopify/callback", ipLimiter, oauthHandler.HandleShopifyCallback)
	app.Get("/auth/woocommerce/start", ipLimiter, oauthHandler.HandleWooCommerceAuthStart)
	app.Post("/auth/woocommerce/callback", ipLimiter, oauthHandler.HandleWooCommerceCallback)
	app.Get("/auth/woocommerce/return", ipLimiter, oauthHandler.HandleWooCommerceReturn)

	// DOOR B: The Omni-Channel Webhook Listeners
	webhookGroup := app.Group("/v1/webhooks")
	
	// Unified Webhook Router
	webhookGroup.Post("/shopify/orders", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopifyOrderCreation)

	// Shopify is our primary driver; it MUST have a secret.
	webhookGroup.Post("/shopify", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopify)
	webhookGroup.Post("/shopify/review", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleProductReview)
	webhookGroup.Post("/shopify/app/uninstalled", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopifyUninstall)
	webhookGroup.Post("/shopify/compliance/customers_data_request", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopifyCustomersDataRequest)
	webhookGroup.Post("/shopify/compliance/customers_redact", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopifyCustomersRedact)
	webhookGroup.Post("/shopify/compliance/shop_redact", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleShopifyShopRedact)

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

	app.Get("/v1/merchants/insights",
		middleware.RequireAPIKey(postgresClient, redisClient),
		analyticsHandler.GetMerchantInsights,
	)

	// Razorpay Billing Integration
	app.Post("/v1/billing/order",
		middleware.RequireAPIKey(postgresClient, redisClient),
		billingHandler.CreateWalletTopUp,
	)
	app.Post("/v1/billing/verify",
		middleware.RequireAPIKey(postgresClient, redisClient),
		billingHandler.VerifyPaymentAndActivate,
	)

	app.Post("/v1/billing/module/purchase",
		middleware.RequireAPIKey(postgresClient, redisClient),
		billingHandler.PurchaseModule,
	)
	app.Post("/v1/billing/module/verify",
		middleware.RequireAPIKey(postgresClient, redisClient),
		billingHandler.VerifyModulePurchase,
	)

	// DOOR C: Private Admin Backdoor 
	adminGroup := app.Group("/v1/admin")
	adminGroup.Use(middleware.RequireIPRateLimit(redisClient, 30))
	adminGroup.Use(middleware.RequireAdminKey())

	adminGroup.Post("/onboard", adminHandler.OnboardMerchant)
	adminGroup.Post("/onboard/magento", magentoHandler.HandleMagentoOnboard)
	adminGroup.Post("/unblock", adminHandler.GetRecentBlocks)
	adminGroup.Post("/import-csv/validate", adminHandler.ValidateCSV)
	adminGroup.Post("/import-csv/commit", adminHandler.CommitCSV)

	// Admin Billing routes
	adminGroup.Get("/billing/events", adminHandler.GetBillingEvents)
	adminGroup.Get("/billing/summary", adminHandler.GetBillingSummary)
	adminGroup.Get("/billing/invoices", adminHandler.GetInvoices)
	adminGroup.Post("/billing/events/:event_id/override", adminHandler.OverrideEventFee)
	adminGroup.Get("/subscriptions", adminHandler.GetSubscriptionStatus)
	adminGroup.Post("/backfill/retrigger-all", adminHandler.RetriggerAllBackfills)
	adminGroup.Get("/merchants/sync-quality", adminHandler.GetMerchantSyncQuality)

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
		slog.Info("Starting RTO Intelligence API on port 8080...")
		if err := app.Listen(":8080"); err != nil {
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