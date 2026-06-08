package main

import (
	"fmt"
	"log"

	"github.com/gofiber/fiber/v2"

	// Ensure your database package is imported here!
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/service"
)

func main() {
	app := fiber.New()

	// 1. Boot up the Database Connection (This runs ONCE at startup)
	redisClient := database.NewRedisClient()
	postgresClient:= database.NewPostgresClient()
	// 2. Instantiate the NEW Redis-powered Engine
	// Notice how we pass the live connection pointer into the service!
	trustSvc := service.NewRedisTrustService(redisClient,postgresClient)

	// 3. Put the Engine in the Car
	// The handler doesn't care that we swapped Mock for Redis! The interface protects it.
	trustHandler := handlers.NewTrustHandler(trustSvc)

	webhookHandler:=handlers.NewWebhookHandler(trustSvc)
	app.Post("v1/webhooks/shopify/return",webhookHandler.HandleOrderReturn)
	fmt.Println("--> Successfully registered POST /v1/trust route!")
	app.Post("/v1/trust", trustHandler.HandleTrustScore)

	log.Println("Starting RTO Intelligence API on port 3000...")
	err := app.Listen(":3000")
	if err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
