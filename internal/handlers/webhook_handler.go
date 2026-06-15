package handlers

import (
	"log/slog"

	"github.com/arpitmandhotra/api-integrator/internal/service"
	 "github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
)

// WebhookHandler handles automated events pushed from Shopify
type WebhookHandler struct {
	trustService service.TrustService
}

func NewWebhookHandler(trustSvc service.TrustService) *WebhookHandler {
	return &WebhookHandler{trustService: trustSvc}
}

// HandleOrderReturn catches Shopify's "Order Returned" event
func (h *WebhookHandler) HandleOrderReturn(c *fiber.Ctx) error {
	// 1. Shopify sends a slightly different payload for webhooks.
	// We'll define a quick struct just for this incoming event.
var payload domain.WebhookPayload

phoneHash := crypto.HashPhone(payload.Phone)
	// 2. Parse the incoming Shopify JSON
	if err := c.BodyParser(&payload); err != nil {
		slog.Error("webhook invalid json", 
	"error", err, 
	"ip", c.IP(),
)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid webhook payload"})
	}

	// 3. Right now, to keep it simple, we will just log that we caught the scammer.
	// In the next step, we will write a function to actually save this to Redis.
	// 3. Extract the context and tell the Service to save the scammer!
	ctx := c.UserContext()
	err := h.trustService.ReportBadActor(ctx, phoneHash, payload.Reason)
	if err != nil {
		// Even if the database fails, we still return 200 OK so Shopify doesn't panic and retry.
		slog.Error("webhook database failure", 
	"error", err, 
	"phone_hash", phoneHash[:8]+"...",
)
	}

	// 4. Webhook Golden Rule: ALWAYS return a 200 OK fast.
	// If you don't reply instantly, Shopify thinks your server crashed and will keep retrying.
	return c.SendStatus(fiber.StatusOK)
}
