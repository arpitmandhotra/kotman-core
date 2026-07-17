package handlers

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/backfill"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type MagentoOnboardHandler struct {
	pg *gorm.DB
}

func NewMagentoOnboardHandler(pg *gorm.DB) *MagentoOnboardHandler {
	return &MagentoOnboardHandler{pg: pg}
}

type MagentoOnboardRequest struct {
	StoreName        string `json:"store_name"`
	StoreBaseURL     string `json:"store_base_url"`
	IntegrationToken string `json:"integration_token"`
}

func (h *MagentoOnboardHandler) HandleMagentoOnboard(c *fiber.Ctx) error {
	var req MagentoOnboardRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid JSON body",
		})
	}

	req.StoreName = strings.TrimSpace(req.StoreName)
	req.StoreBaseURL = strings.TrimSpace(req.StoreBaseURL)
	req.IntegrationToken = strings.TrimSpace(req.IntegrationToken)

	if req.StoreName == "" || req.StoreBaseURL == "" || req.IntegrationToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "store_name, store_base_url, and integration_token are required",
		})
	}

	// Clean trailing slash and validate store url
	baseURL := strings.TrimSpace(req.StoreBaseURL)
	baseURL = strings.TrimSuffix(baseURL, "/")
	if !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://") {
		baseURL = "https://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Magento stores must use HTTPS secure connection",
		})
	}

	ctx := c.UserContext()
	_, _, err = backfill.VerifyMagentoConnection(ctx, baseURL, req.IntegrationToken)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	// Fetch oldest order date
	oldestOrderDate, err := backfill.FetchMagentoOldestOrderDate(ctx, baseURL, req.IntegrationToken)
	if err != nil {
		slog.Warn("failed to fetch oldest Magento order date, defaulting to now", "error", err)
		oldestOrderDate = time.Now().UTC()
	}

	// Encrypt the Magento integration token
	encToken, err := crypto.EncryptToken(req.IntegrationToken)
	if err != nil {
		slog.Error("failed encrypting Magento integration token", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal credential processing error",
		})
	}

	// Generate kt_live_ key
	apiKey, err := GenerateAPIKey()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed generating credentials",
		})
	}

	merchantID := uuid.New().String()

	// Transaction to insert Merchant + PlatformCredential
	err = h.pg.Transaction(func(tx *gorm.DB) error {
		merchant := domain.Merchant{
			ID:               merchantID,
			StoreName:        req.StoreName,
			APIKeyHash:       crypto.HashAPIKey(apiKey),
			Platform:         "magento",
			MagentoBaseURL:   baseURL,
			MagentoCreatedAt: &oldestOrderDate,
			IsActive:         true,
		}
		if err := tx.Create(&merchant).Error; err != nil {
			return err
		}

		settings := domain.MerchantSettings{
			MerchantID: merchantID,
		}
		if err := tx.Create(&settings).Error; err != nil {
			return err
		}

		cred := domain.PlatformCredential{
			MerchantID:                merchantID,
			Platform:                  "magento",
			ShopDomain:                baseURL,
			IntegrationTokenEncrypted: encToken,
			InstalledAt:               time.Now(),
			IsActive:                  true,
		}
		if err := tx.Create(&cred).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		slog.Error("failed database transaction for Magento onboard", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to commit merchant configurations",
		})
	}

	if enqErr := backfill.MagentoBackfillQueue.Enqueue(context.Background(), merchantID); enqErr != nil {
		slog.Error("failed to enqueue magento backfill", "merchant_id", merchantID, "error", enqErr)
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":     "Magento store onboarded successfully",
		"merchant_id": merchantID,
		"api_key":     apiKey,
	})
}
