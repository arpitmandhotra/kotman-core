package database

import (
	"log"
     "os"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
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

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	log.Println("--> Running PostgreSQL Auto-Migration...")
	if err := db.AutoMigrate(&domain.BadActorRecord{}); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}
	
	return db
}