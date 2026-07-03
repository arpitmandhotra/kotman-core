package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arpitmandhotra/api-integrator/internal/handlers"
	"github.com/arpitmandhotra/api-integrator/internal/middleware"
	"github.com/gofiber/fiber/v2"
)

func TestShopifyWebhookRejectsInvalidHMAC(t *testing.T) {
	app := fiber.New()
	
	// Create mock WebhookHandler (passing nil/dummy values since middleware intercepts the request first)
	h := handlers.NewWebhookHandler(nil, nil, "shopify_secret", "", "")

	// Setup webhook group to match main.go
	webhookGroup := app.Group("/v1/webhooks")
	webhookGroup.Post("/shopify", middleware.RequireShopifyHMAC("shopify_secret"), h.HandleShopify)

	req := httptest.NewRequest("POST", "/v1/webhooks/shopify", strings.NewReader(`{"test":true}`))
	req.Header.Set("X-Shopify-Hmac-Sha256", "invalid_hmac_signature")
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed to execute request: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 Unauthorized, got %d", resp.StatusCode)
	}
}

func TestShopifyWebhookRejectsMissingHMAC(t *testing.T) {
	app := fiber.New()
	
	h := handlers.NewWebhookHandler(nil, nil, "shopify_secret", "", "")

	webhookGroup := app.Group("/v1/webhooks")
	webhookGroup.Post("/shopify", middleware.RequireShopifyHMAC("shopify_secret"), h.HandleShopify)

	req := httptest.NewRequest("POST", "/v1/webhooks/shopify", strings.NewReader(`{"test":true}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("failed to execute request: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 Unauthorized, got %d", resp.StatusCode)
	}
}
