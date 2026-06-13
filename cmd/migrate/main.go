package main

import (
	"log"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

func main() {
	log.Println("🚀 Starting Kotman Core Migration CLI...")

	// 1. Connect to the database (This uses your perfectly configured connection pool)
	db := database.NewPostgresClient()

	// 2. Run the migrations
	log.Println("📦 Syncing domain.Merchant schema...")
	if err := db.AutoMigrate(&domain.Merchant{}); err != nil {
		log.Fatalf("❌ Failed to migrate Merchant: %v", err)
	}

	log.Println("📦 Syncing domain.BadActorRecord schema...")
	if err := db.AutoMigrate(&domain.BadActorRecord{}); err != nil {
		log.Fatalf("❌ Failed to migrate BadActorRecord: %v", err)
	}

	log.Println("✅ Database schema perfectly synchronized. Safe to deploy.")
}