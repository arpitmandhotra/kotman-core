package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/service"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type AdminHandler struct {
	pg     *gorm.DB
	csvSvc *service.CSVImportService
}

func NewAdminHandler(pg *gorm.DB, csvSvc *service.CSVImportService) *AdminHandler {
	return &AdminHandler{pg: pg, csvSvc: csvSvc}
}

// ValidateCSV handles POST /v1/admin/import-csv/validate
func (h *AdminHandler) ValidateCSV(c *fiber.Ctx) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing 'file' in form-data",
		})
	}

	platform := c.Query("platform", "generic")
	if formPlatform := c.FormValue("platform"); formPlatform != "" {
		platform = formPlatform
	}

	file, err := fileHeader.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to open file stream",
		})
	}
	defer file.Close()

	report, err := h.csvSvc.ValidateAndStage(c.Context(), file, platform)
	if err != nil {
		slog.Error("CSV validation failed", "error", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(report)
}

// CommitCSVRequest matches the expected body for /commit
type CommitCSVRequest struct {
	PreviewToken string `json:"preview_token"`
	Platform     string `json:"platform"`
}

// CommitCSV handles POST /v1/admin/import-csv/commit
func (h *AdminHandler) CommitCSV(c *fiber.Ctx) error {
	var req CommitCSVRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid JSON body",
		})
	}

	if req.PreviewToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing 'preview_token'",
		})
	}

	platform := req.Platform
	if platform == "" {
		platform = c.Query("platform", "generic")
	}

	merchantID, ok := c.Locals("kotman.merchant_id").(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "missing or invalid merchant context",
		})
	}

	result, err := h.csvSvc.Commit(c.Context(), req.PreviewToken, merchantID, platform)
	if err != nil {
		slog.Error("CSV commit failed", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(result)
}

// GetRecentBlocks fetches the latest scammers caught by the Kotman engine
func (h *AdminHandler) GetRecentBlocks(c *fiber.Ctx) error {
	merchantName := "Admin"
	var scammers []domain.TrustProfile

	// Reach into Cold Storage and grab the 50 most recently caught scammers
	err := h.pg.Order("locked_at desc").Limit(50).Find(&scammers).Error
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to retrieve the vault data",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success":      true,
		"merchant":     merchantName,
		"total_blocks": len(scammers),
		"data":         scammers,
	})
}
// OnboardMerchantRequest holds incoming data for creating a new Shopify client
type OnboardMerchantRequest struct {
	StoreName string `json:"store_name"`
}

// GenerateAPIKey generates a cryptographically secure 32-byte API key with prefix kt_live_
func GenerateAPIKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "kt_live_" + hex.EncodeToString(bytes), nil
}

// OnboardMerchant generates a secure API credential and inserts a new merchant profile using UUIDs
func (h *AdminHandler) OnboardMerchant(c *fiber.Ctx) error {
	var req OnboardMerchantRequest
	if err := c.BodyParser(&req); err != nil || req.StoreName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "A valid store_name string is required",
		})
	}

	// 1. Generate 32 bytes of cryptographically secure randomness
	apiKey, err := GenerateAPIKey()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to safely generate crypto random bytes",
		})
	}

	// 3. Assemble the updated Merchant schema
	merchant := domain.Merchant{
		StoreName: req.StoreName,
		APIKey:    apiKey,
		IsActive:  true,
		// ID (UUID string format) and Timestamps are automatically handled by Postgres/Gorm definitions
	}

	// 4. Persistence execution
	if err := h.pg.Create(&merchant).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to commit new merchant credentials to database",
		})
	}

	// 5. Return payload so you can hand this key over to your friend
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":     "Merchant registered successfully",
		"merchant_id": merchant.ID, // Spits back the generated UUID string
		"store_name":  merchant.StoreName,
		"api_key":     merchant.APIKey,
	})
}