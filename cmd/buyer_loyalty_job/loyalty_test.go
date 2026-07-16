package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type TestMerchant struct {
	ID        string `gorm:"primaryKey"`
	StoreName string
	Tier      domain.MerchantTier
	IsActive  bool
	CreatedAt time.Time
}

func (TestMerchant) TableName() string {
	return "merchants"
}

func TestComputeMerchantLoyalty(t *testing.T) {
	// Setup SQLite in-memory database
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}

	err = db.AutoMigrate(
		&TestMerchant{},
		&domain.Order{},
		&domain.BuyerProfile{},
		&domain.BuyerLoyaltySnapshot{},
	)
	if err != nil {
		t.Fatalf("failed to migrate models: %v", err)
	}

	merchantID := uuid.New()
	merchant := TestMerchant{
		ID:        merchantID.String(),
		StoreName: "Test Loyalty Merchant",
		Tier:      domain.TierGrowth,
		IsActive:  true,
		CreatedAt: time.Now().AddDate(0, 0, -45),
	}
	if err := db.Create(&merchant).Error; err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}

	// Seed 60 orders for 55 unique buyers (5 repeat buyers)
	now := time.Now().UTC()
	for i := 1; i <= 55; i++ {
		phone := fmt.Sprintf("+919000000%03d", i)
		phoneNorm := crypto.HashPhone(phone)
		email := fmt.Sprintf("buyer%d@example.com", i)

		orderDaysAgo := -10
		if i == 1 {
			orderDaysAgo = -35
		}

		// Regular order
		o := domain.Order{
			ID:                   uuid.New(),
			MerchantID:           merchantID,
			OrderNumber:          fmt.Sprintf("ORD-%d", i),
			DeliveryStatus:       "delivered",
			CreatedAt:            now.AddDate(0, 0, orderDaysAgo),
			BuyerPhoneNormalized: phoneNorm,
			BuyerEmail:           email,
			Outcome:              "DELIVERED",
		}
		if err := db.Create(&o).Error; err != nil {
			t.Fatalf("failed to seed order: %v", err)
		}

		// Repeat order for first 5 buyers
		if i <= 5 {
			ro := domain.Order{
				ID:                   uuid.New(),
				MerchantID:           merchantID,
				OrderNumber:          fmt.Sprintf("ORD-%d-REP", i),
				DeliveryStatus:       "delivered",
				CreatedAt:            now.AddDate(0, 0, -5),
				BuyerPhoneNormalized: phoneNorm,
				BuyerEmail:           email,
				Outcome:              "DELIVERED",
			}
			if err := db.Create(&ro).Error; err != nil {
				t.Fatalf("failed to seed repeat order: %v", err)
			}
		}
	}

	// Seed buyer profile for one repeat buyer with 0 RTOs, one repeat buyer with 1 RTO
	bp1 := domain.BuyerProfile{
		ID:                 uuid.New(),
		PhoneNormalized:    crypto.HashPhone("+919000000001"),
		NetworkTotalOrders: 2,
		NetworkRTOCount:    0,
		LastUpdatedAt:      now,
	}
	if err := db.Create(&bp1).Error; err != nil {
		t.Fatalf("failed to seed buyer profile: %v", err)
	}

	bp2 := domain.BuyerProfile{
		ID:                 uuid.New(),
		PhoneNormalized:    crypto.HashPhone("+919000000002"),
		NetworkTotalOrders: 4,
		NetworkRTOCount:    2, // abuser with 2 RTOs out of 4 orders (>40% RTO rate)
		LastUpdatedAt:      now,
	}
	if err := db.Create(&bp2).Error; err != nil {
		t.Fatalf("failed to seed buyer profile: %v", err)
	}

	// Seed RTO orders specifically for the abuser to calculate impact
	abuserOrderRTO := domain.Order{
		ID:                   uuid.New(),
		MerchantID:           merchantID,
		OrderNumber:          "ORD-RTO-1",
		CreatedAt:            now.AddDate(0, 0, -4),
		BuyerPhoneNormalized: crypto.HashPhone("+919000000002"),
		Outcome:              "RTO",
	}
	if err := db.Create(&abuserOrderRTO).Error; err != nil {
		t.Fatalf("failed to seed abuser RTO order: %v", err)
	}

	// Debug print orders
	var totalOrdersCount int64
	db.Model(&domain.Order{}).Count(&totalOrdersCount)
	t.Logf("Total orders in DB: %d", totalOrdersCount)

	var allOrders []domain.Order
	db.Find(&allOrders)
	if len(allOrders) > 0 {
		t.Logf("Sample Order: ID=%s, MerchantID=%s, CreatedAt=%s, Phone=%s, Outcome=%s",
			allOrders[0].ID, allOrders[0].MerchantID, allOrders[0].CreatedAt, allOrders[0].BuyerPhoneNormalized, allOrders[0].Outcome)
	}

	// Compute loyalty metrics
	ctx := context.Background()
	err = computeMerchantLoyalty(ctx, db, merchantID)
	if err != nil {
		t.Fatalf("computeMerchantLoyalty failed: %v", err)
	}

	// Assert computed values
	var snapshot domain.BuyerLoyaltySnapshot
	err = db.Where("merchant_id = ?", merchantID).First(&snapshot).Error
	if err != nil {
		t.Fatalf("failed to find snapshot in DB: %v", err)
	}

	if !snapshot.HasSufficientData {
		t.Errorf("expected has_sufficient_data to be true")
	}

	if snapshot.TotalUniqueBuyers != 55 {
		t.Errorf("expected 55 unique buyers, got %d", snapshot.TotalUniqueBuyers)
	}

	if snapshot.RepeatBuyers != 4 {
		t.Errorf("expected 4 repeat buyers, got %d", snapshot.RepeatBuyers)
	}

	// repeat rate should be (4/55)*100 = 7.3%
	expectedRepeatRate := 7.3
	if snapshot.RepeatRatePct != expectedRepeatRate {
		t.Errorf("expected repeat rate %f, got %f", expectedRepeatRate, snapshot.RepeatRatePct)
	}

	// repeat RTO abusers: buyer 2 has 4 network orders, 2 RTOs (50% RTO rate), and ordered from this merchant.
	// So repeat_rto_abuser_count should be 1.
	if snapshot.RepeatRTOAbuserCount != 1 {
		t.Errorf("expected 1 repeat RTO abuser, got %d", snapshot.RepeatRTOAbuserCount)
	}

	if snapshot.RepeatRTOAbuserTotalRTOs != 1 {
		t.Errorf("expected 1 abuser RTO, got %d", snapshot.RepeatRTOAbuserTotalRTOs)
	}

	expectedCost := 210
	if snapshot.RepeatRTOAbuserEstimatedCostINR != expectedCost {
		t.Errorf("expected RTO abuser cost %d, got %d", expectedCost, snapshot.RepeatRTOAbuserEstimatedCostINR)
	}
}

// Simple fmt stub to fix compiler import
var _ = fmt.Printf
