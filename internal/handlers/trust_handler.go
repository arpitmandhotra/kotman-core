package handlers

import (
	"log/slog"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/service"
	"github.com/gofiber/fiber/v2"
)

type TrustHandler struct {
	trustService service.TrustService
}

func NewTrustHandler(trustSvc service.TrustService) *TrustHandler {
	return &TrustHandler{trustService: trustSvc}
}

func (h *TrustHandler) HandleTrustScore(c *fiber.Ctx) error {
	type TrustScoreRequest struct {
		Phone     string  `json:"phone"`
		IPAddress string  `json:"ip_address"`
		SessionID string  `json:"session_id"`
		CartValue float64 `json:"cart_value"`
	}

	var req TrustScoreRequest
	if err := c.BodyParser(&req); err != nil {
		// M11 FIX: Use slog consistently across all handlers.
		slog.Warn("trust score: failed to parse request body", "error", err, "ip", c.IP())
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	merchantID, ok := c.Locals("kaughtman.merchant_id").(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Missing or invalid merchant context"})
	}

	// M9 FIX: Validate phone before hashing.
	// An empty string always produces the same hash, conflating all anonymous
	// buyers into a single profile and polluting the trust network.
	if req.Phone == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "phone is required"})
	}

	phoneHash := crypto.HashPhone(req.Phone)
	ctx := c.UserContext()

	resp, err := h.trustService.EvaluateRisk(ctx, phoneHash, req.IPAddress, merchantID, req.CartValue)
	if err != nil {
		// M10 FIX: Never return raw internal error messages to clients.
		// They may contain SQL details, connection strings, or stack traces.
		slog.Error("trust score: risk evaluation failed", "error", err, "merchant_id", merchantID)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Trust evaluation temporarily unavailable"})
	}

	return c.JSON(resp)
}
