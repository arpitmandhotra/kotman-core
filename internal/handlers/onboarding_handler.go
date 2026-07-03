package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type OnboardingHandler struct {
	pg *gorm.DB
}

func NewOnboardingHandler(pgDB *gorm.DB) *OnboardingHandler {
	return &OnboardingHandler{
		pg: pgDB,
	}
}

type RegisterRequest struct {
	StoreName string `json:"store_name"`
	Email     string `json:"email"`
}

type RegisterResponse struct {
	APIKey     string `json:"api_key"`
	WebhookURL string `json:"webhook_url"`
}

// RegisterMerchant handles merchant zero-touch registration
func (h *OnboardingHandler) RegisterMerchant(c *fiber.Ctx) error {
	var req RegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "Invalid request body",
		})
	}

	if req.StoreName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "store_name is required",
		})
	}

	// Generate a secure, 32-character API key (8 characters prefix + 24 characters hex)
	keyBytes := make([]byte, 12)
	if _, err := rand.Read(keyBytes); err != nil {
		slog.Error("failed to generate secure random bytes for API key", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to generate API Key",
		})
	}
	apiKey := "sk_live_" + hex.EncodeToString(keyBytes)

	// Hash the API key for safe database storage
	hashedKey := crypto.HashAPIKey(apiKey)

	// Create domain.Merchant record
	merchant := domain.Merchant{
		StoreName:        req.StoreName,
		APIKeyHash:       hashedKey,
		IsActive:         false, // enforces Shadow Mode natively
		ShadowModeEndsAt: time.Now().Add(25 * 24 * time.Hour),
	}

	if err := h.pg.WithContext(c.UserContext()).Create(&merchant).Error; err != nil {
		slog.Error("failed to create merchant record during registration", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to register merchant",
		})
	}

	response := RegisterResponse{
		APIKey:     apiKey,
		WebhookURL: "https://api.yourdomain.com/v1/webhooks/shadow",
	}

	return c.Status(fiber.StatusCreated).JSON(response)
}
