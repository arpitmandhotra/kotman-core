package database

import (
	"log"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// NewPostgresClient creates and returns a connected Postgres database engine
func NewPostgresClient() *gorm.DB {
	// This DSN (Data Source Name) matches the exact credentials you gave Docker
	dsn := "host=localhost user=kotman password=secret dbname=kotman_db port=5432 sslmode=disable TimeZone=Asia/Kolkata"
	
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to Postgres Vault: %v", err)
	}

	log.Println("--> Successfully connected to PostgreSQL Cold Storage!")
	// Tell GORM to build the tables based on our structs!
	log.Println("--> Running PostgreSQL Auto-Migration...")
	if err := db.AutoMigrate(&domain.BadActorRecord{}); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}
	return db
}