package handlers

import (
	"log"

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
	// 1. Expand the struct to catch the IP Address from Postman
	type TrustScoreRequest struct {
		PhoneHash string `json:"phone_hash"`
		IPAddress string `json:"ip_address"` // NEW
		SessionID string `json:"session_id"`
	}

	var req TrustScoreRequest
	if err := c.BodyParser(&req); err != nil {
		log.Println("Error parsing JSON:", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	ctx := c.UserContext()

	// 2. Pass BOTH the phone hash and the IP Address into the brain
	resp, err := h.trustService.EvaluateRisk(ctx, req.PhoneHash, req.IPAddress)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to evaluate risk"})
	}

	return c.JSON(resp)
}
