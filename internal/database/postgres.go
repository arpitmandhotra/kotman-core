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

	// 2. If it's empty, use your exact local Kotman configuration
	if dsn == "" {
		dsn = "host=localhost user=kotman password=secret dbname=kotman_db port=5432 sslmode=disable TimeZone=Asia/Kolkata"
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
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database schema: %v", err)
	}

	// NEW MIGRATION: Add CHECK constraint to protect wallet balance from underflow.
	// Since GORM maps MerchantSettings to the 'merchant_settings' table, we apply it there.
	// We also try 'merchants' table just in case to strictly satisfy the exact SQL request.
	var count int
	db.Raw("SELECT count(*) FROM pg_constraint WHERE conname = 'check_positive_balance'").Scan(&count)
	if count == 0 {
		log.Println("📦 Adding CHECK constraint check_positive_balance to protect ledger integrity...")
		// Try applying it to merchant_settings where the wallet_balance column is actually located in GORM
		db.Exec("ALTER TABLE merchant_settings ADD CONSTRAINT check_positive_balance CHECK (wallet_balance >= 0)")
		// Also try merchants table as requested
		db.Exec("ALTER TABLE merchants ADD CONSTRAINT check_positive_balance CHECK (wallet_balance >= 0)")
	}

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	log.Println("--> Database Schema Auto-Migrated Successfully")
	log.Println("--> Connection Pool Active (Max: 25, Idle: 5)")
	
	return db
}