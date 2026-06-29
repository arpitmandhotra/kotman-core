package handlers

import (
	"log"
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
	// 1. Expand the struct to catch the IP Address from Postman and cart value
	type TrustScoreRequest struct {
		Phone     string  `json:"phone"`
		IPAddress string  `json:"ip_address"` // NEW
		SessionID string  `json:"session_id"`
		CartValue float64 `json:"cart_value"`
	}

	var req TrustScoreRequest
	if err := c.BodyParser(&req); err != nil {
		log.Println("Error parsing JSON:", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	merchantID, ok := c.Locals("kotman.merchant_id").(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Missing or invalid merchant context"})
	}

	phoneHash := crypto.HashPhone(req.Phone)
	ctx := c.UserContext()

	// 2. Pass phone hash, IP Address, merchant ID, and cart value into the service
	resp, err := h.trustService.EvaluateRisk(ctx, phoneHash, req.IPAddress, merchantID, req.CartValue)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(resp)
}
