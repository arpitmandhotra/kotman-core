package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/security"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CatalogWebhookHandler struct {
	pg *gorm.DB
}

func NewCatalogWebhookHandler(pgDB *gorm.DB) *CatalogWebhookHandler {
	return &CatalogWebhookHandler{pg: pgDB}
}

type ShopifyWebhookProduct struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	ProductType string `json:"product_type"`
	Tags        string `json:"tags"`
	Variants    []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		SKU   string `json:"sku"`
		Price string `json:"price"`
	} `json:"variants"`
}

// IngestProductUpsert handles Shopify products/create and products/update webhooks.
// Route: POST /v1/webhooks/catalog/products/upsert
func (h *CatalogWebhookHandler) IngestProductUpsert(c *fiber.Ctx) error {
	merchantParam := c.Query("merchant_id")
	if merchantParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing merchant_id"})
	}
	merchantID, err := uuid.Parse(merchantParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid merchant_id"})
	}

	rawBody := c.Body()
	ctx := c.UserContext()

	// 1. Verify Signature
	err = h.verifySignature(ctx, c, rawBody, merchantID)
	if err != nil {
		slog.Warn("shopify catalog signature verification failed", "error", err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized signature"})
	}

	var product ShopifyWebhookProduct
	if err := json.Unmarshal(rawBody, &product); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON payload"})
	}

	// 2. Perform GORM upsert within transaction
	err = h.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var catalogItems []CatalogProduct
		for _, v := range product.Variants {
			priceVal, _ := strconv.ParseFloat(v.Price, 64)
			catalogItems = append(catalogItems, CatalogProduct{
				ID:                uuid.New(),
				MerchantID:        merchantID,
				Platform:          PlatformShopify,
				PlatformProductID: strconv.FormatInt(product.ID, 10),
				PlatformVariantID: strconv.FormatInt(v.ID, 10),
				SKU:               v.SKU,
				Title:             fmt.Sprintf("%s - %s", product.Title, v.Title),
				CategoryL1:        product.ProductType,
				CategoryL2:        "",
				Price:             Decimal(priceVal),
				IsArchived:        false,
				LastSyncedAt:      time.Now(),
			})
		}

		if len(catalogItems) > 0 {
			err = tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "merchant_id"}, {Name: "platform"}, {Name: "platform_variant_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"sku", "title", "category_l1", "price", "is_archived", "last_synced_at"}),
			}).Create(&catalogItems).Error
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		slog.Error("failed upserting webhook products to catalog", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "database storage failed"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true})
}

// IngestProductDelete handles Shopify products/delete webhooks. Soft-deletes catalog items (IsArchived = true).
// Route: POST /v1/webhooks/catalog/products/delete
func (h *CatalogWebhookHandler) IngestProductDelete(c *fiber.Ctx) error {
	merchantParam := c.Query("merchant_id")
	if merchantParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing merchant_id"})
	}
	merchantID, err := uuid.Parse(merchantParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid merchant_id"})
	}

	rawBody := c.Body()
	ctx := c.UserContext()

	// 1. Verify Signature
	err = h.verifySignature(ctx, c, rawBody, merchantID)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized signature"})
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}

	// 2. Soft-delete variants (IsArchived = true) - DO NOT hard-delete to maintain snapshot join validity
	productIDStr := strconv.FormatInt(payload.ID, 10)
	err = h.pg.WithContext(ctx).Model(&CatalogProduct{}).
		Where("merchant_id = ? AND platform_product_id = ?", merchantID, productIDStr).
		Update("is_archived", true).Error

	if err != nil {
		slog.Error("failed archiving products on deletion webhook", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "archiving database failure"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true})
}

func (h *CatalogWebhookHandler) verifySignature(ctx context.Context, c *fiber.Ctx, body []byte, merchantID uuid.UUID) error {
	hmacHeader := c.Get("X-Shopify-Hmac-Sha256")
	if hmacHeader == "" {
		return errors.New("missing X-Shopify-Hmac-Sha256 signature header")
	}

	// Fetch credentials
	var creds domain.PlatformCredential
	err := h.pg.WithContext(ctx).
		Where("merchant_id = ? AND platform = 'shopify' AND is_active = true", merchantID.String()).
		First(&creds).Error
	if err != nil {
		return fmt.Errorf("failed retrieving shopify credentials: %w", err)
	}

	masterKeyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		return errors.New("TOKEN_ENCRYPTION_KEY is unset")
	}
	masterKeyBytes, err := base64.StdEncoding.DecodeString(masterKeyStr)
	if err != nil {
		return errors.New("invalid base64 master key")
	}

	decryptedSecret, err := security.DecryptString(creds.WebhookSecretEncrypted, masterKeyBytes)
	if err != nil {
		decryptedSecret = creds.WebhookSecretEncrypted
	}

	if !security.ValidateHMAC(body, hmacHeader, []byte(decryptedSecret)) {
		return errors.New("invalid hmac signature")
	}
	return nil
}
