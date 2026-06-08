package main

import (
	"fmt"
	"log"

	"github.com/gofiber/fiber/v2"

	// Ensure your database package is imported here!
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/service"
    "github.com/arpitmandhotra/api-integrator/internal/middleware"    
)

func main() {
	
// ==========================================
	// 1. THE DATABASE LAYER
	// ==========================================
	redisClient := database.NewRedisClient()
	postgresClient := database.NewPostgresClient()

	postgresClient.AutoMigrate(&domain.Merchant{})
	postgresClient.FirstOrCreate(&domain.Merchant{
		StoreName: "Arpit's Test Store",
		APIKey:    "kotman_live_998877665544",
	}, domain.Merchant{APIKey: "kotman_live_998877665544"})

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

	// --> THE SHIELD WALL <--
	app.Use(middleware.RequestLogger())             // 1. Log traffic
	app.Use(middleware.SecurityBouncer())           // 2. Block spammers
	app.Use(middleware.RequireAPIKey(postgresClient)) // 3. Protect the engine

	// ==========================================
	// 4. THE ROUTES
	// ==========================================
	app.Post("/v1/webhooks/shopify/return", webhookHandler.HandleOrderReturn)
	fmt.Println("--> Successfully registered POST /v1/trust route!")
	app.Post("/v1/trust", trustHandler.HandleTrustScore)
   app.Get("/v1/admin/blocks", adminHandler.GetRecentBlocks)
	// ==========================================
	// 5. START UP
	// ==========================================
	log.Println("Starting RTO Intelligence API on port 3000...")
	err := app.Listen(":3000")
	if err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
