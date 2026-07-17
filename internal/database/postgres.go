package database

import (
	"log"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func NewPostgresClient() *gorm.DB {
	// 1. Try to fetch the secure Neon URL from AWS
	dsn := os.Getenv("DATABASE_URL")

	// 2. If it's empty, fail immediately
	if dsn == "" {
		log.Fatal("CRITICAL: DATABASE_URL environment variable is not set. " +
			"For local dev, set: DATABASE_URL=postgres://kotman:yourpassword@localhost:5432/kotman_db?sslmode=disable")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to Postgres Vault: %v", err)
	}

	// ==========================================
	// THE SHIELD: CONNECTION POOL CONFIGURATION
	// ==========================================
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("Failed to get raw DB object for pooling: %v", err)
	}

	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	// ==========================================
	// SCHEMA MIGRATION: Creating the Tables
	// ==========================================
	err = db.AutoMigrate(
		&domain.Merchant{},
		&domain.MerchantSettings{},
		&domain.TrustProfile{},
		&domain.CustomerFeedback{},
		&domain.TransactionHistory{},
		&domain.PlatformCredential{},
		&domain.BackfilledOrder{},
		&domain.BillableEvent{},
		&domain.MerchantInvoice{},
		&domain.MerchantBillingAccumulator{},
		&domain.OrderAudit{},
		&domain.MerchantSubscription{},
		&domain.WhatsAppMessageLog{},
		&domain.MerchantScore{},
		&domain.ScoreComponent{},
		&domain.AWBMapping{},
		&domain.NormalizedDeliveryEvent{},
		&domain.ProcessedWebhookEvent{},
		&domain.CatalogProduct{},
		&domain.Order{},
		&domain.OrderLineItem{},
		&domain.BuyerProfile{},
		&domain.BuyerLoyaltySnapshot{},
		&domain.PincodeReference{},
		&domain.ShopifyBulkOperation{},
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database schema: %v", err)
	}

	// Configure merchant tier check constraints
	alterMerchantSQL := `
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS tier VARCHAR(30) NOT NULL DEFAULT 'free';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS subscription_started_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS subscription_renews_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants DROP CONSTRAINT IF EXISTS chk_merchants_tier;
		ALTER TABLE merchants ADD CONSTRAINT chk_merchants_tier CHECK (tier IN ('free', 'growth', 'growth_ads'));
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_status VARCHAR(50) NOT NULL DEFAULT 'not_started';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_started_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_completed_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_order_count INT NOT NULL DEFAULT 0;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_horizon_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT '1970-01-01 00:00:00+00';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS backfill_error_message TEXT NOT NULL DEFAULT '';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS shopify_created_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS woo_created_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS magento_created_at TIMESTAMP WITH TIME ZONE;
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS magento_base_url VARCHAR(255) NOT NULL DEFAULT '';
	`
	if err := db.Exec(alterMerchantSQL).Error; err != nil {
		log.Fatalf("Failed to migrate Merchant tier columns and constraints: %v", err)
	}

	alterMerchantSettingsSQL := `
		ALTER TABLE merchant_settings ADD COLUMN IF NOT EXISTS capi_dataset_id VARCHAR(100) NOT NULL DEFAULT '';
	`
	if err := db.Exec(alterMerchantSettingsSQL).Error; err != nil {
		log.Fatalf("Failed to migrate MerchantSettings columns: %v", err)
	}

	uniqueIndexSQL := `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_buyer_loyalty_snapshots_merchant_date 
		ON buyer_loyalty_snapshots (merchant_id, (period_end_at::date));
	`
	if err := db.Exec(uniqueIndexSQL).Error; err != nil {
		log.Fatalf("Failed to migrate BuyerLoyaltySnapshot unique index: %v", err)
	}

	alterOrderSQL := `
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS buyer_phone_normalized VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS buyer_email VARCHAR(200) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS outcome VARCHAR(50) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS fulfillment_status VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS payment_method VARCHAR(50) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS order_value_paise INT NOT NULL DEFAULT 0;
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS shipping_address_pincode VARCHAR(20) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS city VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS state VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS geo_state VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS geo_tier VARCHAR(50) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS geo_district VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS geo_latitude DECIMAL(10,7) NOT NULL DEFAULT 0.0;
		ALTER TABLE orders ADD COLUMN IF NOT EXISTS geo_longitude DECIMAL(10,7) NOT NULL DEFAULT 0.0;
	`
	if err := db.Exec(alterOrderSQL).Error; err != nil {
		log.Fatalf("Failed to migrate Order buyer columns: %v", err)
	}

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	log.Println("--> Database Schema Auto-Migrated Successfully")
	log.Println("--> Connection Pool Active (Max: 25, Idle: 5)")
	
	return db
}