package handlers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

var magentoHttpClient = &http.Client{
	Timeout: 10 * time.Second,
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

	// Validate store_base_url is well-formed https:// URL
	u, err := url.Parse(req.StoreBaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid store_base_url: must be a secure https:// URL",
		})
	}

	// Clean trailing slash
	baseURL := strings.TrimSuffix(req.StoreBaseURL, "/")

	// Test the integration token
	testURL := fmt.Sprintf("%s/rest/V1/store/storeConfigs", baseURL)
	ctx, cancel := context.WithTimeout(c.UserContext(), 10*time.Second)
	defer cancel()

	testReq, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to construct Magento validation request",
		})
	}
	testReq.Header.Set("Authorization", "Bearer "+req.IntegrationToken)
	testReq.Header.Set("Content-Type", "application/json")

	resp, err := magentoHttpClient.Do(testReq)
	if err != nil {
		slog.Error("failed testing Magento integration token", "error", err, "url", testURL)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed connecting to Magento store base URL",
		})
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("Magento token validation request rejected", "status", resp.StatusCode)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid Magento integration token — verify it has not expired and has Sales/Orders read permissions",
		})
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
			ID:        merchantID,
			StoreName: req.StoreName,
			APIKey:    apiKey,
			Platform:  "magento",
			IsActive:  true,
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

	// Kick off historical backfill async
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if backfillErr := backfill.BackfillOrderHistory(backfillCtx, merchantID, "magento"); backfillErr != nil {
			slog.Error("magento historical order backfill failed", "merchant_id", merchantID, "error", backfillErr)
		}
	}()

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":     "Magento store onboarded successfully",
		"merchant_id": merchantID,
		"api_key":     apiKey,
	})
}
