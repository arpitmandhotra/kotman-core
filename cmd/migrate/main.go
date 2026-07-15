package main

import (
	"log"

	"github.com/arpitmandhotra/api-integrator/internal/classification"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	log.Println("🚀 Starting Kaughtman Core Migration CLI...")

	// 1. Connect to the database (This uses your perfectly configured connection pool)
	db := database.NewPostgresClient()

	// ═══════════════════════════════════════════════════════════════
	// PHASE 0 — Namespace isolation: create signals + clients schemas
	// These are genuine Postgres schemas (namespaces), NOT tables in public.
	// This lets us grant a data engineer read access to signals.* without
	// ever touching public.merchants or public.billable_events.
	// ═══════════════════════════════════════════════════════════════
	log.Println("📦 Phase 0: Creating signals and clients schemas...")
	schemaSQL := `
		CREATE SCHEMA IF NOT EXISTS signals;
		CREATE SCHEMA IF NOT EXISTS clients;
	`
	if err := db.Exec(schemaSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create schemas: %v", err)
	}
	log.Println("✅ Phase 0: signals + clients schemas ready")

	// ═══════════════════════════════════════════════════════════════
	// PHASE 1 — AutoMigrate all domain models (idempotent — GORM's
	// AutoMigrate adds missing columns, never drops existing ones)
	// ═══════════════════════════════════════════════════════════════
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

	log.Println("📦 Syncing domain.BillableEvent schema (includes new signals columns: IsRTO, CategoryL1, CategoryL2, GeoState, GeoTier)...")
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

	log.Println("📦 Syncing domain.AWBMapping schema...")
	if err := db.AutoMigrate(&domain.AWBMapping{}); err != nil {
		log.Fatalf("❌ Failed to migrate AWBMapping: %v", err)
	}

	log.Println("📦 Syncing domain.NormalizedDeliveryEvent schema...")
	if err := db.AutoMigrate(&domain.NormalizedDeliveryEvent{}); err != nil {
		log.Fatalf("❌ Failed to migrate NormalizedDeliveryEvent: %v", err)
	}

	log.Println("📦 Syncing domain.ProcessedWebhookEvent schema...")
	if err := db.AutoMigrate(&domain.ProcessedWebhookEvent{}); err != nil {
		log.Fatalf("❌ Failed to migrate ProcessedWebhookEvent: %v", err)
	}

	log.Println("📦 Syncing domain.WhatsAppMessageLog schema...")
	if err := db.AutoMigrate(&domain.WhatsAppMessageLog{}); err != nil {
		log.Fatalf("❌ Failed to migrate WhatsAppMessageLog: %v", err)
	}

	log.Println("📦 Syncing domain.CatalogProduct schema...")
	if err := db.AutoMigrate(&domain.CatalogProduct{}); err != nil {
		log.Fatalf("❌ Failed to migrate CatalogProduct: %v", err)
	}

	log.Println("📦 Syncing domain.Order schema...")
	if err := db.AutoMigrate(&domain.Order{}); err != nil {
		log.Fatalf("❌ Failed to migrate Order: %v", err)
	}

	log.Println("📦 Syncing domain.OrderLineItem schema...")
	if err := db.AutoMigrate(&domain.OrderLineItem{}); err != nil {
		log.Fatalf("❌ Failed to migrate OrderLineItem: %v", err)
	}

	// Phase 2 cache table — lives in public schema alongside billable_events
	log.Println("📦 Syncing classification.ProductCategoryCache schema...")
	if err := db.AutoMigrate(&classification.ProductCategoryCache{}); err != nil {
		log.Fatalf("❌ Failed to migrate ProductCategoryCache: %v", err)
	}

	log.Println("✅ Phase 1: All domain models synchronized (including new signals columns)")

	// ═══════════════════════════════════════════════════════════════
	// PHASE 3 — Signal tables (schema: signals)
	// Daily grain with window_days discriminator. All tables are
	// schema-qualified (signals.*). Uses IF NOT EXISTS for idempotency.
	// ═══════════════════════════════════════════════════════════════
	log.Println("📦 Phase 3: Creating signal tables in signals schema...")

	categorySignalsSQL := `
		CREATE TABLE IF NOT EXISTS signals.category_signals (
			id              BIGSERIAL PRIMARY KEY,
			category_l1     VARCHAR(50) NOT NULL,
			category_l2     VARCHAR(50) NOT NULL,
			geo_state       VARCHAR(50) NOT NULL,
			geo_tier        SMALLINT NOT NULL,
			snapshot_date   DATE NOT NULL,
			window_days     SMALLINT NOT NULL,
			order_count     INT NOT NULL,
			merchant_count  INT NOT NULL,
			gmv_indexed     NUMERIC(10,2) NOT NULL,
			rto_rate        NUMERIC(5,4) NOT NULL,
			cod_share       NUMERIC(5,4) NOT NULL,
			aov_indexed     NUMERIC(10,2) NOT NULL,
			computed_at     TIMESTAMP NOT NULL DEFAULT now(),
			UNIQUE (category_l1, category_l2, geo_state, geo_tier, snapshot_date, window_days)
		);
	`
	if err := db.Exec(categorySignalsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create signals.category_signals: %v", err)
	}

	geoSignalsSQL := `
		CREATE TABLE IF NOT EXISTS signals.geo_signals (
			id              BIGSERIAL PRIMARY KEY,
			geo_state       VARCHAR(50) NOT NULL,
			geo_tier        SMALLINT NOT NULL,
			snapshot_date   DATE NOT NULL,
			window_days     SMALLINT NOT NULL,
			order_count     INT NOT NULL,
			merchant_count  INT NOT NULL,
			gmv_indexed     NUMERIC(10,2) NOT NULL,
			rto_rate        NUMERIC(5,4) NOT NULL,
			cod_share       NUMERIC(5,4) NOT NULL,
			computed_at     TIMESTAMP NOT NULL DEFAULT now(),
			UNIQUE (geo_state, geo_tier, snapshot_date, window_days)
		);
	`
	if err := db.Exec(geoSignalsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create signals.geo_signals: %v", err)
	}

	paymentSignalsSQL := `
		CREATE TABLE IF NOT EXISTS signals.payment_signals (
			id                    BIGSERIAL PRIMARY KEY,
			category_l1           VARCHAR(50) NOT NULL,
			geo_state             VARCHAR(50) NOT NULL,
			snapshot_date         DATE NOT NULL,
			window_days           SMALLINT NOT NULL,
			cod_share             NUMERIC(5,4) NOT NULL,
			prepaid_share         NUMERIC(5,4) NOT NULL,
			cod_share_change_2d   NUMERIC(6,4),
			merchant_count        INT NOT NULL,
			computed_at           TIMESTAMP NOT NULL DEFAULT now(),
			UNIQUE (category_l1, geo_state, snapshot_date, window_days)
		);
	`
	if err := db.Exec(paymentSignalsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create signals.payment_signals: %v", err)
	}

	indexBasePeriodsSQL := `
		CREATE TABLE IF NOT EXISTS signals.index_base_periods (
			cohort_key      VARCHAR(200) PRIMARY KEY,
			base_gmv        NUMERIC(14,2) NOT NULL,
			base_aov        NUMERIC(10,2) NOT NULL,
			base_date       DATE NOT NULL,
			locked_at       TIMESTAMP NOT NULL DEFAULT now()
		);
	`
	if err := db.Exec(indexBasePeriodsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create signals.index_base_periods: %v", err)
	}

	log.Println("✅ Phase 3: All signal tables created in signals schema")

	// ═══════════════════════════════════════════════════════════════
	// PHASE 5 — Client access layer (schema: clients)
	// Schema-only: tables prepared for future API routes. No HTTP
	// handlers or auth middleware built yet.
	// ═══════════════════════════════════════════════════════════════
	log.Println("📦 Phase 5: Creating client access tables in clients schema...")

	dataClientsSQL := `
		CREATE TABLE IF NOT EXISTS clients.data_clients (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_name        VARCHAR(200) NOT NULL,
			org_type        VARCHAR(30) NOT NULL,
			api_key_hash    VARCHAR(128) UNIQUE NOT NULL,
			contact_email   VARCHAR(200) NOT NULL,
			is_active       BOOLEAN NOT NULL DEFAULT true,
			created_at      TIMESTAMP NOT NULL DEFAULT now()
		);
	`
	if err := db.Exec(dataClientsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create clients.data_clients: %v", err)
	}

	clientSubscriptionsSQL := `
		CREATE TABLE IF NOT EXISTS clients.client_subscriptions (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			client_id       UUID NOT NULL REFERENCES clients.data_clients(id),
			dataset_id      VARCHAR(50) NOT NULL,
			tier            VARCHAR(30) NOT NULL DEFAULT 'standard',
			valid_from      DATE NOT NULL,
			valid_until     DATE NOT NULL,
			is_active       BOOLEAN NOT NULL DEFAULT true,
			UNIQUE (client_id, dataset_id)
		);
	`
	if err := db.Exec(clientSubscriptionsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create clients.client_subscriptions: %v", err)
	}

	apiAccessLogsSQL := `
		CREATE TABLE IF NOT EXISTS clients.api_access_logs (
			id              BIGSERIAL PRIMARY KEY,
			client_id       UUID NOT NULL REFERENCES clients.data_clients(id),
			dataset_id      VARCHAR(50) NOT NULL,
			endpoint        VARCHAR(200) NOT NULL,
			rows_returned   INT NOT NULL,
			called_at       TIMESTAMP NOT NULL DEFAULT now()
		);
	`
	if err := db.Exec(apiAccessLogsSQL).Error; err != nil {
		log.Fatalf("❌ Failed to create clients.api_access_logs: %v", err)
	}

	log.Println("✅ Phase 5: All client access tables created in clients schema")

	// 3. One-time warning check for unhashed legacy API keys
	var unhashedMerchants []domain.Merchant
	if err := db.Where("api_key_hash IS NULL OR api_key_hash = ''").Find(&unhashedMerchants).Error; err == nil {
		for _, m := range unhashedMerchants {
			log.Printf("⚠️ WARNING: Cannot auto-hash existing keys for merchant %s (ID: %s) — raw keys are not recoverable from the DB. Rotate these keys manually via /v1/admin/onboard", m.StoreName, m.ID)
		}
	}

	log.Println("✅ Database schema perfectly synchronized. Safe to deploy.")
	log.Println("   ├── public.*: Core Kaughtman tables (Merchant, BillableEvent, etc.)")
	log.Println("   ├── signals.*: Category, Geo, Payment signal tables + index base periods")
	log.Println("   └── clients.*: Data client access tables (schema-only, no API routes yet)")
}

