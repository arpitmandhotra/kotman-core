package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	postgres_driver "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// createSignalsSchema runs the DDL to set up the signals schema and all
// required tables inside a test database. This mirrors the production
// migration.
func createSignalsSchema(t *testing.T, db *gorm.DB, isSQLite bool) {
	t.Helper()

	var ddl []string
	if isSQLite {
		ddl = []string{
			`ATTACH DATABASE ':memory:' AS signals`,
			`ATTACH DATABASE ':memory:' AS clients`,
			`CREATE TABLE IF NOT EXISTS billable_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				merchant_id VARCHAR(100) NOT NULL,
				order_id VARCHAR(100),
				platform VARCHAR(50) DEFAULT 'shopify',
				checkout_mode VARCHAR(50) DEFAULT 'native',
				payment_method VARCHAR(20) NOT NULL,
				order_value_paise INT NOT NULL,
				fee_paise INT DEFAULT 0,
				is_billable BOOLEAN NOT NULL DEFAULT true,
				is_rto BOOLEAN NOT NULL DEFAULT false,
				category_l1 VARCHAR(50) NOT NULL DEFAULT '',
				category_l2 VARCHAR(50) NOT NULL DEFAULT '',
				geo_state VARCHAR(50) NOT NULL DEFAULT '',
				geo_tier SMALLINT NOT NULL DEFAULT 0,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS signals.index_base_periods (
				cohort_key VARCHAR(200) PRIMARY KEY,
				base_gmv NUMERIC(14,2) NOT NULL,
				base_aov NUMERIC(10,2) NOT NULL,
				base_date TEXT NOT NULL,
				locked_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS signals.category_signals (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				category_l1 VARCHAR(50) NOT NULL,
				category_l2 VARCHAR(50) NOT NULL,
				geo_state VARCHAR(50) NOT NULL,
				geo_tier SMALLINT NOT NULL,
				snapshot_date TEXT NOT NULL,
				window_days SMALLINT NOT NULL,
				order_count INT NOT NULL,
				merchant_count INT NOT NULL,
				gmv_indexed NUMERIC(10,2) NOT NULL,
				rto_rate NUMERIC(5,4) NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				aov_indexed NUMERIC(10,2) NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE (category_l1, category_l2, geo_state, geo_tier, snapshot_date, window_days)
			)`,
			`CREATE TABLE IF NOT EXISTS signals.geo_signals (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				geo_state VARCHAR(50) NOT NULL,
				geo_tier SMALLINT NOT NULL,
				snapshot_date TEXT NOT NULL,
				window_days SMALLINT NOT NULL,
				order_count INT NOT NULL,
				merchant_count INT NOT NULL,
				gmv_indexed NUMERIC(10,2) NOT NULL,
				rto_rate NUMERIC(5,4) NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE (geo_state, geo_tier, snapshot_date, window_days)
			)`,
			`CREATE TABLE IF NOT EXISTS signals.payment_signals (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				category_l1 VARCHAR(50) NOT NULL,
				geo_state VARCHAR(50) NOT NULL,
				snapshot_date TEXT NOT NULL,
				window_days SMALLINT NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				prepaid_share NUMERIC(5,4) NOT NULL,
				cod_share_change_2d NUMERIC(6,4),
				merchant_count INT NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE (category_l1, geo_state, snapshot_date, window_days)
			)`,
		}
	} else {
		ddl = []string{
			`CREATE SCHEMA IF NOT EXISTS signals`,
			`CREATE TABLE IF NOT EXISTS billable_events (
				id BIGSERIAL PRIMARY KEY,
				merchant_id VARCHAR(100) NOT NULL,
				order_id VARCHAR(100),
				platform VARCHAR(50) DEFAULT 'shopify',
				checkout_mode VARCHAR(50) DEFAULT 'native',
				payment_method VARCHAR(20) NOT NULL,
				order_value_paise INT NOT NULL,
				fee_paise INT DEFAULT 0,
				is_billable BOOLEAN NOT NULL DEFAULT true,
				is_rto BOOLEAN NOT NULL DEFAULT false,
				category_l1 VARCHAR(50) NOT NULL DEFAULT '',
				category_l2 VARCHAR(50) NOT NULL DEFAULT '',
				geo_state VARCHAR(50) NOT NULL DEFAULT '',
				geo_tier SMALLINT NOT NULL DEFAULT 0,
				created_at TIMESTAMP NOT NULL DEFAULT now()
			)`,
			`CREATE TABLE IF NOT EXISTS signals.index_base_periods (
				cohort_key VARCHAR(200) PRIMARY KEY,
				base_gmv NUMERIC(14,2) NOT NULL,
				base_aov NUMERIC(10,2) NOT NULL,
				base_date DATE NOT NULL,
				locked_at TIMESTAMP NOT NULL DEFAULT now()
			)`,
			`CREATE TABLE IF NOT EXISTS signals.category_signals (
				id BIGSERIAL PRIMARY KEY,
				category_l1 VARCHAR(50) NOT NULL,
				category_l2 VARCHAR(50) NOT NULL,
				geo_state VARCHAR(50) NOT NULL,
				geo_tier SMALLINT NOT NULL,
				snapshot_date DATE NOT NULL,
				window_days SMALLINT NOT NULL,
				order_count INT NOT NULL,
				merchant_count INT NOT NULL,
				gmv_indexed NUMERIC(10,2) NOT NULL,
				rto_rate NUMERIC(5,4) NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				aov_indexed NUMERIC(10,2) NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT now(),
				UNIQUE (category_l1, category_l2, geo_state, geo_tier, snapshot_date, window_days)
			)`,
			`CREATE TABLE IF NOT EXISTS signals.geo_signals (
				id BIGSERIAL PRIMARY KEY,
				geo_state VARCHAR(50) NOT NULL,
				geo_tier SMALLINT NOT NULL,
				snapshot_date DATE NOT NULL,
				window_days SMALLINT NOT NULL,
				order_count INT NOT NULL,
				merchant_count INT NOT NULL,
				gmv_indexed NUMERIC(10,2) NOT NULL,
				rto_rate NUMERIC(5,4) NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT now(),
				UNIQUE (geo_state, geo_tier, snapshot_date, window_days)
			)`,
			`CREATE TABLE IF NOT EXISTS signals.payment_signals (
				id BIGSERIAL PRIMARY KEY,
				category_l1 VARCHAR(50) NOT NULL,
				geo_state VARCHAR(50) NOT NULL,
				snapshot_date DATE NOT NULL,
				window_days SMALLINT NOT NULL,
				cod_share NUMERIC(5,4) NOT NULL,
				prepaid_share NUMERIC(5,4) NOT NULL,
				cod_share_change_2d NUMERIC(6,4),
				merchant_count INT NOT NULL,
				computed_at TIMESTAMP NOT NULL DEFAULT now(),
				UNIQUE (category_l1, geo_state, snapshot_date, window_days)
			)`,
		}
	}

	for _, stmt := range ddl {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("DDL execution failed: %s\nStatement: %s", err, stmt)
		}
	}
}

func TestSignalsAggregation_Integration(t *testing.T) {
	t.Setenv("SIGNALS_MIN_MERCHANT_COUNT", "1")
	ctx := context.Background()

	var db *gorm.DB
	isSQLite := false

	t.Log("Booting isolated Postgres via testcontainers...")

	pgContainer, err := tcpostgres.Run(ctx, "postgres:15-alpine",
		tcpostgres.WithDatabase("kotman_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Logf("Docker/Testcontainers not available: %s. Falling back to in-memory SQLite.", err)
		isSQLite = true
		sqliteDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			t.Fatalf("failed to open in-memory SQLite: %s", err)
		}
		db = sqliteDB
	} else {
		defer func() {
			if err := pgContainer.Terminate(ctx); err != nil {
				t.Logf("warning: failed to terminate Postgres container: %s", err)
			}
		}()

		connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("Could not get Postgres connection string: %s", err)
		}
		t.Logf("Postgres is live at %s", connStr)

		postgresDB, err := gorm.Open(postgres_driver.Open(connStr), &gorm.Config{})
		if err != nil {
			t.Fatalf("failed to connect to Postgres: %s", err)
		}
		db = postgresDB
	}

	createSignalsSchema(t, db, isSQLite)

	// Prepare time references (IST)
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("failed to load IST timezone: %s", err)
	}
	now := time.Now().In(ist)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)
	yesterday := today.AddDate(0, 0, -1).Add(12 * time.Hour) // noon yesterday

	// Seed test data:
	// 6 merchants in Apparel > Ethnic Wear > Maharashtra > Tier 1 (should PASS k-anonymity)
	for i := 1; i <= 6; i++ {
		err := db.Exec(`INSERT INTO billable_events (merchant_id, payment_method, order_value_paise, is_billable, category_l1, category_l2, geo_state, geo_tier, created_at)
			VALUES (?, 'cod', 150000, true, 'Apparel', 'Ethnic Wear', 'Maharashtra', 1, ?)`,
			fmt.Sprintf("merchant_%d", i), yesterday).Error
		if err != nil {
			t.Fatalf("failed to seed Apparel data: %s", err)
		}
	}

	// Only 3 merchants in Electronics > Smartphones > Delhi > Tier 1 (should FAIL k-anonymity)
	for i := 1; i <= 3; i++ {
		err := db.Exec(`INSERT INTO billable_events (merchant_id, payment_method, order_value_paise, is_billable, category_l1, category_l2, geo_state, geo_tier, created_at)
			VALUES (?, 'prepaid', 250000, true, 'Electronics', 'Smartphones', 'Delhi', 1, ?)`,
			fmt.Sprintf("merchant_%d", i), yesterday).Error
		if err != nil {
			t.Fatalf("failed to seed Electronics data: %s", err)
		}
	}

	t.Run("K_Anonymity_Drops_Low_Merchant_Count", func(t *testing.T) {
		if err := RunAggregation(db, today, 1); err != nil {
			t.Fatalf("RunAggregation failed: %v", err)
		}

		// Assert: Electronics should NOT appear in category_signals (only 3 merchants < 5)
		var count int64
		db.Raw(`SELECT COUNT(*) FROM signals.category_signals WHERE category_l1 = 'Electronics' AND window_days = 1`).Scan(&count)
		if count != 0 {
			t.Errorf("expected 0 rows for Electronics (k-anonymity should drop), got %d", count)
		}

		// Assert: Delhi should NOT appear in geo_signals (only 3 merchants < 5)
		var geoCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.geo_signals WHERE geo_state = 'Delhi' AND window_days = 1`).Scan(&geoCount)
		if geoCount != 0 {
			t.Errorf("expected 0 rows for Delhi (k-anonymity should drop), got %d", geoCount)
		}

		// Assert: Electronics should NOT appear in payment_signals
		var payCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.payment_signals WHERE category_l1 = 'Electronics' AND window_days = 1`).Scan(&payCount)
		if payCount != 0 {
			t.Errorf("expected 0 rows for payment signals under Electronics, got %d", payCount)
		}
	})

	t.Run("K_Anonymity_Passes_High_Merchant_Count", func(t *testing.T) {
		// Assert: Apparel SHOULD appear in category_signals (6 merchants >= 5)
		var count int64
		db.Raw(`SELECT COUNT(*) FROM signals.category_signals WHERE category_l1 = 'Apparel' AND window_days = 1`).Scan(&count)
		if count == 0 {
			t.Errorf("expected rows for Apparel (6 merchants >= k-anonymity threshold), got 0")
		}

		// Assert: First run = base period, so gmv_indexed should be 100.00
		var gmvIndexed float64
		db.Raw(`SELECT gmv_indexed FROM signals.category_signals WHERE category_l1 = 'Apparel' AND window_days = 1 LIMIT 1`).Scan(&gmvIndexed)
		if gmvIndexed != 100.00 {
			t.Errorf("expected gmv_indexed=100.00 for first run (base period), got %.2f", gmvIndexed)
		}

		// Also verify aov_indexed = 100.00
		var aovIndexed float64
		db.Raw(`SELECT aov_indexed FROM signals.category_signals WHERE category_l1 = 'Apparel' AND window_days = 1 LIMIT 1`).Scan(&aovIndexed)
		if aovIndexed != 100.00 {
			t.Errorf("expected aov_indexed=100.00 for first run (base period), got %.2f", aovIndexed)
		}

		// Verify geo_signals were also written and indexed properly
		var geoCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.geo_signals WHERE geo_state = 'Maharashtra' AND window_days = 1`).Scan(&geoCount)
		if geoCount == 0 {
			t.Errorf("expected geo_signals rows for Maharashtra, got 0")
		}

		var geoGMVIndexed float64
		db.Raw(`SELECT gmv_indexed FROM signals.geo_signals WHERE geo_state = 'Maharashtra' AND window_days = 1 LIMIT 1`).Scan(&geoGMVIndexed)
		if geoGMVIndexed != 100.00 {
			t.Errorf("expected geo gmv_indexed=100.00 for first run (base period), got %.2f", geoGMVIndexed)
		}

		// Verify payment_signals were written
		var payCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.payment_signals WHERE category_l1 = 'Apparel' AND geo_state = 'Maharashtra' AND window_days = 1`).Scan(&payCount)
		if payCount == 0 {
			t.Errorf("expected payment_signals rows for Apparel|Maharashtra, got 0")
		}
	})

	t.Run("Idempotent_Upsert_On_Rerun", func(t *testing.T) {
		// Count rows BEFORE re-run
		var beforeCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.category_signals WHERE window_days = 1`).Scan(&beforeCount)

		var beforeOrderCount int64
		db.Raw(`SELECT order_count FROM signals.category_signals WHERE category_l1 = 'Apparel' AND window_days = 1 LIMIT 1`).Scan(&beforeOrderCount)

		// Re-run the same aggregation
		if err := RunAggregation(db, today, 1); err != nil {
			t.Fatalf("second RunAggregation failed: %v", err)
		}

		// Assert: Row count should NOT increase (upsert, not insert)
		var afterCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.category_signals WHERE window_days = 1`).Scan(&afterCount)
		if afterCount != beforeCount {
			t.Errorf("expected idempotent upsert (row count %d → %d), got increase", beforeCount, afterCount)
		}

		// Assert: Values should be identical
		var afterOrderCount int64
		db.Raw(`SELECT order_count FROM signals.category_signals WHERE category_l1 = 'Apparel' AND window_days = 1 LIMIT 1`).Scan(&afterOrderCount)
		if afterOrderCount != beforeOrderCount {
			t.Errorf("expected same order_count after re-run (%d), got %d", beforeOrderCount, afterOrderCount)
		}

		// Verify geo_signals are also idempotent
		var geoBeforeCount, geoAfterCount int64
		db.Raw(`SELECT COUNT(*) FROM signals.geo_signals WHERE window_days = 1`).Scan(&geoBeforeCount)
		db.Raw(`SELECT COUNT(*) FROM signals.geo_signals WHERE window_days = 1`).Scan(&geoAfterCount)
		if geoAfterCount != geoBeforeCount {
			t.Errorf("geo_signals not idempotent: %d → %d", geoBeforeCount, geoAfterCount)
		}
	})
}

func TestSignalsAggregation_MinMerchantDensityGuard(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory SQLite: %s", err)
	}

	createSignalsSchema(t, db, true)

	// Set min merchant count threshold to 5 for test purposes
	t.Setenv("SIGNALS_MIN_MERCHANT_COUNT", "5")

	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("failed to load IST timezone: %s", err)
	}
	now := time.Now().In(ist)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)
	yesterday := today.AddDate(0, 0, -1).Add(12 * time.Hour)

	// Seed with 4 merchants (below threshold of 5)
	for i := 1; i <= 4; i++ {
		err := db.Exec(`INSERT INTO billable_events (merchant_id, payment_method, order_value_paise, is_billable, category_l1, category_l2, geo_state, geo_tier, created_at)
			VALUES (?, 'cod', 150000, true, 'Apparel', 'Ethnic Wear', 'Maharashtra', 1, ?)`,
			fmt.Sprintf("merchant_%d", i), yesterday).Error
		if err != nil {
			t.Fatalf("failed to seed Apparel data: %s", err)
		}
	}

	// Run aggregation
	err = RunAggregation(db, today, 1)
	if err != nil {
		t.Fatalf("expected RunAggregation to return nil (clean skip), got error: %v", err)
	}

	// Assert: signals.category_signals should have 0 rows
	var count int64
	db.Raw(`SELECT COUNT(*) FROM signals.category_signals`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 rows in category_signals due to density guard, got %d", count)
	}

	// Assert: signals.geo_signals should have 0 rows
	db.Raw(`SELECT COUNT(*) FROM signals.geo_signals`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 rows in geo_signals due to density guard, got %d", count)
	}
}

