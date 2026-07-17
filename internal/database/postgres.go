package database

import (
	"log"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
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
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS email VARCHAR(255) NOT NULL DEFAULT '';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS password_hash VARCHAR(255) NOT NULL DEFAULT '';
		ALTER TABLE merchants ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT false;
		ALTER TABLE merchants ALTER COLUMN api_key_hash DROP NOT NULL;
	`
	if err := db.Exec(alterMerchantSQL).Error; err != nil {
		log.Fatalf("Failed to migrate Merchant columns and constraints: %v", err)
	}

	// Update existing rows (registered before verification) to avoid locking them out
	if err := db.Exec("UPDATE merchants SET email_verified = true WHERE email = ''").Error; err != nil {
		log.Fatalf("Failed to update email_verified for existing merchants: %v", err)
	}

	alterMerchantSettingsSQL := `
		ALTER TABLE merchant_settings ADD COLUMN IF NOT EXISTS capi_dataset_id VARCHAR(100) NOT NULL DEFAULT '';
		ALTER TABLE merchant_settings ADD COLUMN IF NOT EXISTS meta_access_token_encrypted TEXT NOT NULL DEFAULT '';
	`
	if err := db.Exec(alterMerchantSettingsSQL).Error; err != nil {
		log.Fatalf("Failed to migrate MerchantSettings columns: %v", err)
	}

	// One-time migration for existing MetaAccessToken plaintext values
	if db.Migrator().HasColumn(&domain.MerchantSettings{}, "meta_access_token") {
		type PlainSettings struct {
			MerchantID      string
			MetaAccessToken string
		}
		var results []PlainSettings
		if err := db.Raw("SELECT merchant_id, meta_access_token FROM merchant_settings WHERE meta_access_token != ''").Scan(&results).Error; err == nil {
			for _, r := range results {
				if enc, encErr := crypto.EncryptToken(r.MetaAccessToken); encErr == nil {
					_ = db.Exec("UPDATE merchant_settings SET meta_access_token_encrypted = ? WHERE merchant_id = ?", enc, r.MerchantID)
				}
			}
		}
		_ = db.Migrator().DropColumn(&domain.MerchantSettings{}, "meta_access_token")
	}

	// Drop shadow_mode_ends_at column from merchants if it exists
	if db.Migrator().HasColumn(&domain.Merchant{}, "shadow_mode_ends_at") {
		_ = db.Migrator().DropColumn(&domain.Merchant{}, "shadow_mode_ends_at")
	}

	uniqueIndexSQL := `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_buyer_loyalty_snapshots_merchant_date 
		ON buyer_loyalty_snapshots (merchant_id, (period_end_at::date));
		CREATE INDEX IF NOT EXISTS idx_loyalty_merchant_period 
		ON buyer_loyalty_snapshots (merchant_id, period_end_at DESC);
		CREATE INDEX IF NOT EXISTS idx_billable_event_invoice_lookup 
		ON billable_events (merchant_id, created_at, invoice_id) 
		WHERE invoice_id = '' OR invoice_id IS NULL;
	`
	if err := db.Exec(uniqueIndexSQL).Error; err != nil {
		log.Fatalf("Failed to migrate unique and composite indexes: %v", err)
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