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
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database schema: %v", err)
	}

	// Note: Prepaid wallet check constraint was removed in postpaid billing migration.

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	log.Println("--> Database Schema Auto-Migrated Successfully")
	log.Println("--> Connection Pool Active (Max: 25, Idle: 5)")
	
	return db
}