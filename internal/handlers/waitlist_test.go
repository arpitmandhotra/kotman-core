package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type TestMerchant struct {
	ID         string `gorm:"primaryKey"`
	Email      string
	APIKeyHash string
	IsActive   bool
}

func (TestMerchant) TableName() string {
	return "merchants"
}

func TestWaitlist(t *testing.T) {
	// Initialize in-memory SQLite database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open SQLite: %v", err)
	}

	if err := db.AutoMigrate(&domain.WaitlistEntry{}, &TestMerchant{}); err != nil {
		t.Fatalf("failed to migrate waitlist tables: %v", err)
	}

	// Create a test merchant
	apiKey := "test_waitlist_key_12345"
	hashedKey := crypto.HashAPIKey(apiKey)
	merchant := TestMerchant{
		ID:         "m-waitlist-123",
		Email:      "merchant@store.com",
		APIKeyHash: hashedKey,
		IsActive:   true,
	}
	if err := db.Create(&merchant).Error; err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}

	app := fiber.New()
	onboardingHandler := handlers.NewOnboardingHandler(db, nil)

	// Register waitlist endpoints
	app.Post("/v1/waitlist/join", onboardingHandler.JoinWaitlist)
	app.Get("/v1/admin/waitlist", onboardingHandler.GetWaitlist)

	t.Run("POST /v1/waitlist/join - anonymous valid", func(t *testing.T) {
		body := `{"email": "visitor@example.com", "store_name": "Visitor Shop", "tier_interest": "growth"}`
		req := httptest.NewRequest("POST", "/v1/waitlist/join", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var resMap map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&resMap)
		if resMap["success"] != true {
			t.Errorf("expected success: true, got %+v", resMap)
		}

		// Verify record exists in DB
		var entry domain.WaitlistEntry
		if err := db.Where("email = ?", "visitor@example.com").First(&entry).Error; err != nil {
			t.Fatalf("could not find waitlist entry in DB: %v", err)
		}
		if entry.Source != "pricing_page" {
			t.Errorf("expected source 'pricing_page', got '%s'", entry.Source)
		}
	})

	t.Run("POST /v1/waitlist/join - logged in merchant", func(t *testing.T) {
		body := `{"email": "merchant@store.com", "store_name": "Merchant Store", "tier_interest": "both"}`
		req := httptest.NewRequest("POST", "/v1/waitlist/join", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", apiKey)

		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var entry domain.WaitlistEntry
		if err := db.Where("email = ?", "merchant@store.com").First(&entry).Error; err != nil {
			t.Fatalf("could not find waitlist entry in DB: %v", err)
		}
		if entry.MerchantID != merchant.ID {
			t.Errorf("expected merchant ID '%s', got '%s'", merchant.ID, entry.MerchantID)
		}
		if entry.Source != "dashboard" {
			t.Errorf("expected source 'dashboard', got '%s'", entry.Source)
		}
	})

	t.Run("POST /v1/waitlist/join - validation errors", func(t *testing.T) {
		// Invalid email
		body := `{"email": "invalid-email", "store_name": "Store", "tier_interest": "growth"}`
		req := httptest.NewRequest("POST", "/v1/waitlist/join", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := app.Test(req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid email, got %d", resp.StatusCode)
		}

		// Invalid tier
		body = `{"email": "test@domain.com", "store_name": "Store", "tier_interest": "invalid_tier"}`
		req = httptest.NewRequest("POST", "/v1/waitlist/join", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ = app.Test(req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid tier, got %d", resp.StatusCode)
		}
	})

	t.Run("POST /v1/waitlist/join - conflict upsert", func(t *testing.T) {
		body := `{"email": "visitor@example.com", "store_name": "Visitor Shop Updated", "tier_interest": "both"}`
		req := httptest.NewRequest("POST", "/v1/waitlist/join", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := app.Test(req)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var entry domain.WaitlistEntry
		if err := db.Where("email = ?", "visitor@example.com").First(&entry).Error; err != nil {
			t.Fatalf("could not find waitlist entry in DB: %v", err)
		}
		if entry.StoreName != "Visitor Shop Updated" {
			t.Errorf("expected updated store name 'Visitor Shop Updated', got '%s'", entry.StoreName)
		}
		if entry.TierInterest != "both" {
			t.Errorf("expected updated tier_interest 'both', got '%s'", entry.TierInterest)
		}
	})

	t.Run("GET /v1/admin/waitlist", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/admin/waitlist?tier=both", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var resMap struct {
			Entries []domain.WaitlistEntry `json:"entries"`
			Total   int64                  `json:"total"`
		}
		json.NewDecoder(resp.Body).Decode(&resMap)

		if resMap.Total != 2 { // visitor@example.com (updated to both) and merchant@store.com (both)
			t.Errorf("expected total 2, got %d", resMap.Total)
		}
		if len(resMap.Entries) != 2 {
			t.Errorf("expected 2 entries in slice, got %d", len(resMap.Entries))
		}
	})
}

func TestGatedBillingEndpoints(t *testing.T) {
	app := fiber.New()
	billingHandler := handlers.NewBillingHandler(nil, nil)

	app.Post("/v1/billing/subscription/activate", billingHandler.ActivateSubscription)
	app.Post("/v1/billing/subscription/upgrade", billingHandler.UpgradeSubscription)

	t.Run("ActivateSubscription is gated", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/billing/subscription/activate", nil)
		resp, _ := app.Test(req)

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", resp.StatusCode)
		}

		var resMap map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&resMap)
		if resMap["code"] != "TIER_NOT_YET_AVAILABLE" {
			t.Errorf("expected code TIER_NOT_YET_AVAILABLE, got %s", resMap["code"])
		}
		if !strings.Contains(resMap["message"].(string), "coming soon") {
			t.Errorf("expected coming soon message, got %s", resMap["message"])
		}
	})

	t.Run("UpgradeSubscription is gated", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/billing/subscription/upgrade", nil)
		resp, _ := app.Test(req)

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", resp.StatusCode)
		}

		var resMap map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&resMap)
		if resMap["code"] != "TIER_NOT_YET_AVAILABLE" {
			t.Errorf("expected code TIER_NOT_YET_AVAILABLE, got %s", resMap["code"])
		}
	})
}
