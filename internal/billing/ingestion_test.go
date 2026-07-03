package billing

import (
	"context"
	"testing"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type TestMerchantSettings struct {
	ID                 string    `gorm:"primaryKey"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
	MerchantID         string    `gorm:"uniqueIndex;not null"`
	CheckoutMode       string    `gorm:"default:'native'"`
	ThirdPartyCheckout string    `gorm:"default:''"`
	BillingCycleDay    int       `gorm:"default:1"`
	AutoInvoiceEnabled bool      `gorm:"default:true"`
	WalletBalance      float64   `gorm:"default:0"`
}

func (TestMerchantSettings) TableName() string {
	return "merchant_settings"
}

func TestProcessInboundOrder_IdempotencyAndAccumulator(t *testing.T) {
	// Setup pure-Go SQLite in-memory database
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}

	// Migrate models (using TestMerchantSettings to bypass gen_random_uuid() constraint on SQLite)
	err = db.AutoMigrate(
		&TestMerchantSettings{},
		&domain.BillableEvent{},
		&domain.MerchantBillingAccumulator{},
	)
	if err != nil {
		t.Fatalf("failed to migrate models: %v", err)
	}

	// Initialize billing package singleton
	DB = db

	// Seed settings
	merchantID := "merchant-123-abc-xyz"
	mSettings := TestMerchantSettings{
		ID:                 "settings-123",
		MerchantID:         merchantID,
		CheckoutMode:       "native",
		ThirdPartyCheckout: "",
		BillingCycleDay:    1,
		AutoInvoiceEnabled: true,
		WalletBalance:      100.0,
	}
	if err := db.Create(&mSettings).Error; err != nil {
		t.Fatalf("failed to seed merchant settings: %v", err)
	}

	// Sample raw payload body (Shopify order)
	rawJSON := `{
		"id": 1122334455,
		"total_price": "249.50",
		"payment_gateway": "manual",
		"billing_address": {
			"phone": "+919876543210"
		},
		"note_attributes": [
			{"name": "kotman_risk", "value": "low"}
		],
		"tags": "some_tag, another",
		"source_name": "web"
	}`

	ctx := context.Background()

	// 1. First execution
	err = ProcessInboundOrder(ctx, "shopify", merchantID, []byte(rawJSON))
	if err != nil {
		t.Fatalf("ProcessInboundOrder failed on first execution: %v", err)
	}

	// Verify BillableEvent was created
	var count int64
	db.Model(&domain.BillableEvent{}).Count(&count)
	if count != 1 {
		t.Errorf("expected 1 BillableEvent row, got %d", count)
	}

	var event domain.BillableEvent
	if err := db.First(&event).Error; err != nil {
		t.Fatalf("failed to query billable event: %v", err)
	}
	if event.OrderID != "1122334455" {
		t.Errorf("expected OrderID to be '1122334455', got %q", event.OrderID)
	}
	if event.FeePaise != 500 {
		t.Errorf("expected FeePaise to be 500, got %d", event.FeePaise)
	}
	if !event.IsBillable {
		t.Errorf("expected event to be billable")
	}

	// Verify Accumulator was created and incremented
	var accum domain.MerchantBillingAccumulator
	month := currentBillingMonth()
	if err := db.Where("merchant_id = ? AND billing_month = ?", merchantID, month).First(&accum).Error; err != nil {
		t.Fatalf("failed to query accumulator: %v", err)
	}
	if accum.TotalEvents != 1 {
		t.Errorf("expected accumulator TotalEvents to be 1, got %d", accum.TotalEvents)
	}
	if accum.TotalFeePaise != 500 {
		t.Errorf("expected accumulator TotalFeePaise to be 500, got %d", accum.TotalFeePaise)
	}

	// 2. Second execution (idempotency check)
	err = ProcessInboundOrder(ctx, "shopify", merchantID, []byte(rawJSON))
	if err != nil {
		t.Fatalf("ProcessInboundOrder failed (should not fail on duplicate): %v", err)
	}

	// Verify no second BillableEvent was created
	db.Model(&domain.BillableEvent{}).Count(&count)
	if count != 1 {
		t.Errorf("expected BillableEvent count to remain 1 after duplicate call, got %d", count)
	}

	// Verify accumulator was NOT incremented again
	if err := db.Where("merchant_id = ? AND billing_month = ?", merchantID, month).First(&accum).Error; err != nil {
		t.Fatalf("failed to query accumulator: %v", err)
	}
	if accum.TotalEvents != 1 {
		t.Errorf("expected accumulator TotalEvents to remain 1, got %d", accum.TotalEvents)
	}
	if accum.TotalFeePaise != 500 {
		t.Errorf("expected accumulator TotalFeePaise to remain 500, got %d", accum.TotalFeePaise)
	}
}

func TestProcessOrderCreditBack(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}

	err = db.AutoMigrate(
		&TestMerchantSettings{},
		&domain.BillableEvent{},
		&domain.MerchantBillingAccumulator{},
	)
	if err != nil {
		t.Fatalf("failed to migrate models: %v", err)
	}

	DB = db

	merchantID := "merchant-456"
	mSettings := TestMerchantSettings{
		ID:                 "settings-456",
		MerchantID:         merchantID,
		CheckoutMode:       "native",
		WalletBalance:      100.0,
	}
	if err := db.Create(&mSettings).Error; err != nil {
		t.Fatalf("failed to seed merchant settings: %v", err)
	}

	ctx := context.Background()
	rawJSON := `{
		"id": 998877,
		"total_price": "150.00",
		"payment_gateway": "cod",
		"billing_address": {
			"phone": "+919876543210"
		}
	}`

	// 1. Process order first to record the event and deduct fee (150 Rupees order triggers tier fee of 500 paise / 5.0 Rupees)
	err = ProcessInboundOrder(ctx, "shopify", merchantID, []byte(rawJSON))
	if err != nil {
		t.Fatalf("ProcessInboundOrder failed: %v", err)
	}

	// Verify balance was deducted (100.0 - 5.0 = 95.0)
	var settings TestMerchantSettings
	db.Where("merchant_id = ?", merchantID).First(&settings)
	if settings.WalletBalance != 95.0 {
		t.Errorf("expected wallet balance to be 95.0 after charge, got %f", settings.WalletBalance)
	}

	// 2. Process credit back (RTO/Cancellation)
	err = ProcessOrderCreditBack(ctx, "shopify", merchantID, "998877")
	if err != nil {
		t.Fatalf("ProcessOrderCreditBack failed: %v", err)
	}

	// Verify balance was refunded (95.0 + 5.0 = 100.0)
	db.Where("merchant_id = ?", merchantID).First(&settings)
	if settings.WalletBalance != 100.0 {
		t.Errorf("expected wallet balance to be restored to 100.0 after credit back, got %f", settings.WalletBalance)
	}

	// Verify BillableEvent is marked as not billable and has fee = 0
	var event domain.BillableEvent
	if err := db.Where("merchant_id = ? AND order_id = ?", merchantID, "998877").First(&event).Error; err != nil {
		t.Fatalf("failed to query billable event: %v", err)
	}
	if event.IsBillable {
		t.Errorf("expected event to be marked as not billable")
	}
	if event.FeePaise != 0 {
		t.Errorf("expected event fee to be set to 0, got %d", event.FeePaise)
	}

	// Verify Accumulator totals were decremented to 0
	var accum domain.MerchantBillingAccumulator
	month := currentBillingMonth()
	if err := db.Where("merchant_id = ? AND billing_month = ?", merchantID, month).First(&accum).Error; err != nil {
		t.Fatalf("failed to query accumulator: %v", err)
	}
	if accum.TotalEvents != 0 {
		t.Errorf("expected accumulator TotalEvents to be 0, got %d", accum.TotalEvents)
	}
	if accum.TotalFeePaise != 0 {
		t.Errorf("expected accumulator TotalFeePaise to be 0, got %d", accum.TotalFeePaise)
	}
}
