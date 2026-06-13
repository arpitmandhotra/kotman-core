package database

import (
	"log"
	"os"
	"time" // <-- Added for the 5-minute pool timer

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func NewPostgresClient() *gorm.DB {
	// 1. Try to fetch the secure Neon URL from AWS
	dsn := os.Getenv("DATABASE_URL")

	// 2. If it's empty, use your exact local Kotman configuration
	if dsn == "" {
		dsn = "host=host.docker.internal user=kotman password=secret dbname=kotman_db port=5432 sslmode=disable TimeZone=Asia/Kolkata"
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

	// 1. Cap the absolute maximum number of simultaneous open connections
	sqlDB.SetMaxOpenConns(25)
	
	// 2. Keep 5 connections "warm" and idling when traffic is low
	sqlDB.SetMaxIdleConns(5)
	
	// 3. Force connections to recycle every 5 minutes to prevent memory leaks
	sqlDB.SetConnMaxLifetime(5 * time.Minute)
	// ==========================================

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	log.Println("--> Connection Pool Active (Max: 25, Idle: 5)")
	
	log.Println("--> Running PostgreSQL Auto-Migration...")
	if err := db.AutoMigrate(&domain.BadActorRecord{}); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	return db
}