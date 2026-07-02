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

	log.Println("📦 Syncing domain.MerchantSettings schema...")
	if err := db.AutoMigrate(&domain.MerchantSettings{}); err != nil {
		log.Fatalf("❌ Failed to migrate MerchantSettings: %v", err)
	}

	log.Println("📦 Syncing domain.TrustProfile schema...")
	if err := db.AutoMigrate(&domain.TrustProfile{}); err != nil {
		log.Fatalf("❌ Failed to migrate TrustProfile: %v", err)
	}

	log.Println("📦 Syncing domain.CustomerFeedback schema...")
	if err := db.AutoMigrate(&domain.CustomerFeedback{}); err != nil {
		log.Fatalf("❌ Failed to migrate CustomerFeedback: %v", err)
	}

	log.Println("📦 Syncing domain.TransactionHistory schema...")
	if err := db.AutoMigrate(&domain.TransactionHistory{}); err != nil {
		log.Fatalf("❌ Failed to migrate TransactionHistory: %v", err)
	}

	log.Println("📦 Syncing domain.PlatformCredential schema...")
	if err := db.AutoMigrate(&domain.PlatformCredential{}); err != nil {
		log.Fatalf("❌ Failed to migrate PlatformCredential: %v", err)
	}

	log.Println("📦 Syncing domain.BackfilledOrder schema...")
	if err := db.AutoMigrate(&domain.BackfilledOrder{}); err != nil {
		log.Fatalf("❌ Failed to migrate BackfilledOrder: %v", err)
	}

	log.Println("📦 Syncing domain.BillableEvent schema...")
	if err := db.AutoMigrate(&domain.BillableEvent{}); err != nil {
		log.Fatalf("❌ Failed to migrate BillableEvent: %v", err)
	}

	log.Println("📦 Syncing domain.MerchantInvoice schema...")
	if err := db.AutoMigrate(&domain.MerchantInvoice{}); err != nil {
		log.Fatalf("❌ Failed to migrate MerchantInvoice: %v", err)
	}

	log.Println("📦 Syncing domain.MerchantBillingAccumulator schema...")
	if err := db.AutoMigrate(&domain.MerchantBillingAccumulator{}); err != nil {
		log.Fatalf("❌ Failed to migrate MerchantBillingAccumulator: %v", err)
	}

	log.Println("✅ Database schema perfectly synchronized. Safe to deploy.")
}
