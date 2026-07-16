package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func TestGetBuyerIntelligence(t *testing.T) {
	// Initialize in-memory SQLite
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	// AutoMigrate domain.Order
	if err := db.AutoMigrate(&domain.Order{}); err != nil {
		t.Fatalf("failed to migrate orders table: %v", err)
	}

	merchantID := uuid.New()

	// Seed test orders
	now := time.Now().UTC()

	// 1. Ghost Buyer: 1 order, fulfilled, created > 30 days ago
	ghostPhone := "ghost-hash-1"
	db.Create(&domain.Order{
		ID:                     uuid.New(),
		MerchantID:             merchantID,
		OrderNumber:            "ORD-GHOST",
		BuyerPhoneNormalized:   ghostPhone,
		FulfillmentStatus:      "fulfilled",
		CreatedAt:              now.AddDate(0, 0, -35),
		OrderValuePaise:        150000, // ₹1500
		PaymentMethod:          "cod",
	})

	// 2. Prepaid Conversion Candidate:
	// - 3 or more orders with payment_method = "cod"
	// - ALL of their cod orders have fulfillment_status = "fulfilled"
	// - ZERO orders with prepaid
	prepaidCandidatePhone := "prepaid-candidate-hash"
	for i := 1; i <= 3; i++ {
		db.Create(&domain.Order{
			ID:                     uuid.New(),
			MerchantID:             merchantID,
			OrderNumber:            string(rune(i)),
			BuyerPhoneNormalized:   prepaidCandidatePhone,
			FulfillmentStatus:      "fulfilled",
			CreatedAt:              now.AddDate(0, 0, -i),
			OrderValuePaise:        200000, // ₹2000
			PaymentMethod:          "cod",
		})
	}

	// 3. Order Velocity:
	// - phone hashes with 2 or more fulfilled orders
	// - we'll have a buyer with exactly 2 orders, first created 10 days ago, second created 2 days ago (interval = 8 days)
	velocityPhone := "velocity-hash"
	db.Create(&domain.Order{
		ID:                     uuid.New(),
		MerchantID:             merchantID,
		OrderNumber:            "V1",
		BuyerPhoneNormalized:   velocityPhone,
		FulfillmentStatus:      "fulfilled",
		CreatedAt:              now.AddDate(0, 0, -10),
	})
	db.Create(&domain.Order{
		ID:                     uuid.New(),
		MerchantID:             merchantID,
		OrderNumber:            "V2",
		BuyerPhoneNormalized:   velocityPhone,
		FulfillmentStatus:      "fulfilled",
		CreatedAt:              now.AddDate(0, 0, -2),
	})

	// 4. Pincode Health:
	// - 5 orders for a single pincode
	pincode := "110001"
	for i := 1; i <= 5; i++ {
		db.Create(&domain.Order{
			ID:                     uuid.New(),
			MerchantID:             merchantID,
			OrderNumber:            string(rune(i + 100)),
			BuyerPhoneNormalized:   string(rune(i + 500)),
			FulfillmentStatus:      "fulfilled",
			CreatedAt:              now.AddDate(0, 0, -i),
			PaymentMethod:          "cod",
			ShippingAddressPincode: pincode,
			City:                   "New Delhi",
			State:                  "Delhi",
		})
	}

	// Initialize Fiber App
	app := fiber.New()
	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	analyticsHandler := handlers.NewAnalyticsHandler(db, redisClient)

	app.Get("/v1/merchants/buyer-intelligence", func(c *fiber.Ctx) error {
		// Mock RequireAPIKey injecting context locals
		c.Locals("kaughtman.merchant_id", merchantID.String())
		return analyticsHandler.GetBuyerIntelligence(c)
	})

	// Perform HTTP Request
	req := httptest.NewRequest("GET", "/v1/merchants/buyer-intelligence", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed to run fiber app test: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var response handlers.BuyerIntelligenceResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	// Assertions
	if !response.Success {
		t.Error("expected response success to be true")
	}

	// Ghost Buyers
	if response.GhostBuyers.Count != 1 {
		t.Errorf("expected 1 ghost buyer, got %d", response.GhostBuyers.Count)
	}
	if response.GhostBuyers.AvgFirstOrderValueINR != 1500.00 {
		t.Errorf("expected avg_first_order_value_inr 1500.0, got %f", response.GhostBuyers.AvgFirstOrderValueINR)
	}
	if response.GhostBuyers.AvgDaysSinceLastOrder != 35 {
		t.Errorf("expected avg_days_since_last_order 35, got %d", response.GhostBuyers.AvgDaysSinceLastOrder)
	}
	if response.GhostBuyers.RecoverableCount != 0 { // floor(1 * 0.18) = 0
		t.Errorf("expected recoverable_count 0, got %d", response.GhostBuyers.RecoverableCount)
	}

	// Prepaid candidates
	if response.PrepaidCandidates.Count != 1 {
		t.Errorf("expected 1 prepaid candidate, got %d", response.PrepaidCandidates.Count)
	}
	if response.PrepaidCandidates.AvgOrderValueINR != 2000.00 {
		t.Errorf("expected avg_order_value_inr 2000.0, got %f", response.PrepaidCandidates.AvgOrderValueINR)
	}

	// Order velocity
	if response.OrderVelocity.AvgDaysToReorder != 5 {
		t.Errorf("expected avg_days_to_reorder 5, got %d", response.OrderVelocity.AvgDaysToReorder)
	}
	if response.OrderVelocity.OptimalWindowStartDay != 1 { // 5 - 6 = -1, floored to 1
		t.Errorf("expected optimal_window_start_day 1, got %d", response.OrderVelocity.OptimalWindowStartDay)
	}
	if response.OrderVelocity.OptimalWindowEndDay != 8 { // 5 + 3 = 8
		t.Errorf("expected optimal_window_end_day 8, got %d", response.OrderVelocity.OptimalWindowEndDay)
	}

	// Pincode Health
	if len(response.PincodeHealth) != 1 {
		t.Fatalf("expected 1 pincode health metric, got %d", len(response.PincodeHealth))
	}
	p := response.PincodeHealth[0]
	if p.Pincode != "110001" {
		t.Errorf("expected pincode 110001, got %s", p.Pincode)
	}
	if p.City != "New Delhi" || p.State != "Delhi" {
		t.Errorf("expected New Delhi, Delhi, got %s, %s", p.City, p.State)
	}
	if p.TotalOrders != 5 {
		t.Errorf("expected 5 total orders, got %d", p.TotalOrders)
	}
	if p.FulfillmentRate != 1.0 {
		t.Errorf("expected fulfillment rate 1.0, got %f", p.FulfillmentRate)
	}
	if p.Status != "healthy" {
		t.Errorf("expected status healthy, got %s", p.Status)
	}
}
