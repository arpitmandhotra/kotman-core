package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	goredis "github.com/redis/go-redis/v9"
	postgres_driver "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/service"
)

func TestEvaluateRisk_Integration(t *testing.T) {
	ctx := context.Background()

	t.Log("Orchestrating isolated ecosystem (Postgres + Redis)...")

	// ==========================================
	// 1. BOOT REDIS
	// ==========================================
	redisContainer, err := tcredis.RunContainer(ctx,
		testcontainers.WithImage("redis:7-alpine"),
	)
	if err != nil {
		t.Skipf("Skipping integration test: Docker/Testcontainers not available: %s", err)
	}
	defer func() {
		if err := redisContainer.Terminate(ctx); err != nil {
			t.Logf("warning: failed to terminate Redis container: %s", err)
		}
	}()

	redisURI, err := redisContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("CRITICAL FAIL: Could not get Redis connection string: %s", err)
	}
	t.Logf("SUCCESS: Redis is live at %s", redisURI)

	// ==========================================
	// 2. BOOT POSTGRES
	// ==========================================
	pgContainer, err := tcpostgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:15-alpine"),
		tcpostgres.WithDatabase("kotman_test"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(15*time.Second),
		),
	)
	if err != nil {
		t.Skipf("Skipping integration test: Docker/Testcontainers not available: %s", err)
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("warning: failed to terminate Postgres container: %s", err)
		}
	}()

	pgURI, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("CRITICAL FAIL: Could not get Postgres connection string: %s", err)
	}
	t.Logf("SUCCESS: Postgres is live at %s", pgURI)

	// ==========================================
	// 4. WIRE THE REAL CLIENTS
	// ==========================================

	// Wire Redis client from container URI
	redisOpts, err := goredis.ParseURL(redisURI)
	if err != nil {
		t.Fatalf("failed to parse Redis URI: %s", err)
	}
	redisClient := goredis.NewClient(redisOpts)
	defer redisClient.Close()

	// Wire Postgres via GORM from container URI
	pgDB, err := gorm.Open(postgres_driver.Open(pgURI), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect to Postgres: %s", err)
	}

	// Run migrations on the test database
	if err := pgDB.AutoMigrate(
		&domain.TrustProfile{},
		&domain.Merchant{},
		&domain.MerchantSettings{},
		&domain.TransactionHistory{},
		&domain.Order{},
	); err != nil {
		t.Fatalf("failed to migrate test database: %s", err)
	}

	// Create test merchant and settings
	testMerchantID := "f6e5d4c3-b2a1-0987-6543-210fedcba987"
	if err := pgDB.Create(&domain.Merchant{
		ID:         testMerchantID,
		StoreName:  "Test Integration Store",
		APIKeyHash: crypto.HashAPIKey("test_key_123"),
		IsActive:   true,
	}).Error; err != nil {
		t.Fatalf("failed to seed test merchant: %s", err)
	}

	if err := pgDB.Create(&domain.MerchantSettings{
		MerchantID:         testMerchantID,
		WalletBalancePaise: 100000,
	}).Error; err != nil {
		t.Fatalf("failed to seed test merchant settings: %s", err)
	}

	// Build the real service
	svc := service.NewRedisTrustService(redisClient, pgDB)

	// ==========================================
	// 5. THE FIVE TEST CASES
	// ==========================================

	t.Run("CleanUser_ReturnsAllow", func(t *testing.T) {
		resp, err := svc.EvaluateRisk(ctx, "unknown_hash_abc123", "1.2.3.4", testMerchantID, 150.0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Action != "ALLOW_COD" {
			t.Errorf("expected ALLOW_COD, got %s", resp.Action)
		}
		if resp.Score != 85 {
			t.Errorf("expected score 85, got %d", resp.Score)
		}
	})

	t.Run("BadActor_PostgresSeed_ReturnsHide", func(t *testing.T) {
		badHash := "known_bad_actor_hash"
		now := time.Now() // 1. Create the time variable first

		pgDB.Create(&domain.TrustProfile{
			PhoneHash:       badHash,
			IsBlacklisted:   true,                    // Mark as explicitly blocked
			BlacklistReason: "integration test seed", // Changed from Reason
			LockedAt:        &now,                    // Pass the pointer
		})

		resp, err := svc.EvaluateRisk(ctx, badHash, "1.2.3.5", testMerchantID, 150.0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Action != "HIDE_COD" {
			t.Errorf("expected HIDE_COD, got %s", resp.Action)
		}
		if resp.Score != 5 {
			t.Errorf("expected score 5, got %d", resp.Score)
		}

		// Verify cache warming
		val, redisErr := redisClient.Get(ctx, badHash).Result()
		if redisErr != nil {
			t.Errorf("expected bad actor to be cached in Redis after Postgres hit, got error: %v", redisErr)
		}
		if val != "5" {
			t.Errorf("expected Redis cached score '5', got '%s'", val)
		}
	})

	t.Run("RedisCacheHit_SkipsPostgres", func(t *testing.T) {
		cachedHash := "cached_bad_actor"
		redisClient.Set(ctx, cachedHash, "20", 24*time.Hour)

		resp, err := svc.EvaluateRisk(ctx, cachedHash, "1.2.3.6", testMerchantID, 150.0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Action != "HIDE_COD" {
			t.Errorf("expected HIDE_COD, got %s", resp.Action)
		}
	})

	t.Run("VelocityBot_SameIP_ReturnsBlock", func(t *testing.T) {
		botIP := "9.9.9.9"
		botHash := "velocity_test_hash"

		for i := 0; i < 4; i++ {
			svc.EvaluateRisk(ctx, botHash, botIP, testMerchantID, 150.0)
		}

		resp, err := svc.EvaluateRisk(ctx, botHash, botIP, testMerchantID, 150.0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Action != "HIDE_COD" {
			t.Errorf("expected velocity block HIDE_COD, got %s", resp.Action)
		}
		if resp.Score != 10 {
			t.Errorf("expected velocity score 10, got %d", resp.Score)
		}
	})

	t.Run("ReportBadActor_PersistsToPostgres", func(t *testing.T) {
		reportHash := "newly_reported_scammer"
		err := svc.ReportBadActor(ctx, reportHash, "order_returned")
		if err != nil {
			t.Fatalf("ReportBadActor failed: %v", err)
		}

		var record domain.TrustProfile
		result := pgDB.Where("phone_hash = ?", reportHash).First(&record)
		if result.Error != nil {
			t.Errorf("expected bad actor in Postgres, got error: %v", result.Error)
		}
		if record.BlacklistReason != "order_returned" {
			t.Errorf("expected reason 'order_returned', got '%s'", record.BlacklistReason)
		}

		val, _ := redisClient.Get(ctx, reportHash).Result()
		if val != "20" {
			t.Errorf("expected Redis score '20' after report, got '%s'", val)
		}
	})
}
