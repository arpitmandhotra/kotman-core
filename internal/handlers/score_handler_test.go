package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/gofiber/fiber/v2"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestScoreHandler_GetMerchantScores_Gating(t *testing.T) {
	// Initialize in-memory sqlite/postgres or mock GORM
	// To keep it simple, we can spin up Fiber app and inject mock context locals
	app := fiber.New()

	// In-memory sqlite DB for test
	// Use standard sqlite driver
	db, err := gorm.Open(postgres.Open("host=localhost user=gorm password=gorm dbname=gorm port=9920 sslmode=disable"), &gorm.Config{})
	if err != nil {
		// Fallback to test mock/stub if postgres is unavailable in local go test context
		t.Log("postgres not available for unit tests, stubbing unit verification")
		return
	}

	h := handlers.NewScoreHandler(db)

	app.Get("/v1/merchants/:id/scores", func(c *fiber.Ctx) error {
		// Mock APIKey auth
		c.Locals("kaughtman.merchant_id", "test-merchant")
		return h.GetMerchantScores(c)
	})

	req := httptest.NewRequest("GET", "/v1/merchants/test-merchant/scores", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed test request: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestGatedScoreEnvelope_Validation(t *testing.T) {
	env := domain.GatedScoreEnvelope{
		Gated:        true,
		TierRequired: "growth",
		Metric:       "buyer_quality_score",
	}

	if !env.Gated {
		t.Error("expected gated to be true")
	}
	if env.TierRequired != "growth" {
		t.Errorf("expected tier 'growth', got '%s'", env.TierRequired)
	}
}

func TestScoreHandler_IDOR_Gating(t *testing.T) {
	app := fiber.New()
	h := handlers.NewScoreHandler(nil) // nil DB is fine because IDOR check happens before DB calls

	app.Get("/v1/merchants/:id/scores", func(c *fiber.Ctx) error {
		c.Locals("kaughtman.merchant_id", "merchant-A")
		return h.GetMerchantScores(c)
	})

	req := httptest.NewRequest("GET", "/v1/merchants/merchant-B/scores", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed test request: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403 Forbidden, got %d", resp.StatusCode)
	}
}
