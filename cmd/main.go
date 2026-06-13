package main

import (
	  "os/signal"
    "syscall"
	"fmt"
	"log"
	"os"
"time"

	"github.com/gofiber/fiber/v2"

	// Ensure your database package is imported here!
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
	merchantKey:= os.Getenv("MERCHANT_API_KEY")
	if merchantKey==""{
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

	// ==========================================
	// 2. THE SERVICE & HANDLER LAYER
	// ==========================================
	trustSvc := service.NewRedisTrustService(redisClient, postgresClient)
	trustHandler := handlers.NewTrustHandler(trustSvc)
  //  adminHandler := handlers.NewAdminHandler(postgresClient)
	webhookHandler := handlers.NewWebhookHandler(trustSvc)

	// ==========================================
	// 3. THE ROUTER & MIDDLEWARE LAYER
	// ==========================================
	app := fiber.New()

	// --> THE SHIELD WALL <--
	app.Use(middleware.RequestLogger())             // 1. Log traffic
	app.Use(middleware.SecurityBouncer(os.Getenv("REDIS_URL")))           // 2. Block spammers
	

	
	// ==========================================
// 4. THE ROUTES
// ==========================================

// DOOR B: Shopify (Uses Cryptography)
app.Post("/v1/webhooks/shopify/return", middleware.RequireShopifyHMAC(shopifySecret), webhookHandler.HandleOrderReturn)

// DOOR A: Private Enterprise (Uses Database API Keys)
fmt.Println("--> Successfully registered POST /v1/trust route!")
app.Post("/v1/trust", middleware.RequireAPIKey(postgresClient), trustHandler.HandleTrustScore)

app.Get("/health", func(c *fiber.Ctx) error {
    // ping Redis
    _, redisErr := redisClient.Ping(c.UserContext()).Result()
    // ping Postgres  
    sqlDB, _ := postgresClient.DB()
    postgresErr := sqlDB.PingContext(c.UserContext())

    if redisErr != nil || postgresErr != nil {
        return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
            "status": "unhealthy",
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
go func() {
    log.Println("Starting RTO Intelligence API on port 3000...")
    if err := app.Listen(":3000"); err != nil {
        log.Fatalf("Server failed to start: %v", err)
    }
}()

// Create a channel to receive OS signals
quit := make(chan os.Signal, 1)

// Tell Go's signal package: forward SIGTERM and SIGINT to our channel
// SIGTERM = ECS/Kubernetes sends this on deploy/scale-down
// SIGINT  = Ctrl+C in your terminal during local dev
signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

// Block here until a signal arrives
<-quit

log.Println("Shutdown signal received. Draining in-flight requests...")

// Give active requests up to 30 seconds to complete
// After that, Fiber force-closes remaining connections
if err := app.ShutdownWithTimeout(30 * time.Second); err != nil {
    log.Fatalf("Forced shutdown after timeout: %v", err)
}

log.Println("Server shut down cleanly.")
}
