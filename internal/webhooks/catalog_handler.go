package webhooks

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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CatalogWebhookHandler struct {
	pg *gorm.DB
}

func NewCatalogWebhookHandler(pgDB *gorm.DB) *CatalogWebhookHandler {
	return &CatalogWebhookHandler{pg: pgDB}
}

// ShopifyWebhookProduct mirrors the single product payload received in webhooks.
type ShopifyWebhookProduct struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	ProductType string `json:"product_type"`
	Tags        string `json:"tags"`
	Variants    []struct {
		ID        int64  `json:"id"`
		ProductID int64  `json:"product_id"`
		Title     string `json:"title"`
		SKU       string `json:"sku"`
		Price     string `json:"price"`
		CompareAt string `json:"compare_at_price"`
	} `json:"variants"`
}

// HandleProductUpsert handles products/create and products/update webhooks from Shopify.
func (h *CatalogWebhookHandler) HandleProductUpsert(c *fiber.Ctx) error {
	merchantID := c.Query("merchant_id")
	if merchantID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing merchant_id"})
	}

	rawBody := c.Body()
	ctx := c.UserContext()

	// 1. Authenticate signature using Shopify client secret
	err := h.verifyShopifySignature(ctx, c, rawBody, merchantID)
	if err != nil {
		slog.Warn("shopify webhook verification failed", "error", err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized webhook source"})
	}

	var product ShopifyWebhookProduct
	if err := json.Unmarshal(rawBody, &product); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "failed to parse product payload"})
	}

	// 2. Perform DB update inside transaction
	err = h.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var dbVariants []domain.ProductCatalog
		for _, v := range product.Variants {
			pricePaise := parsePriceToPaise(v.Price)
			comparePaise := parsePriceToPaise(v.CompareAt)

			dbVariants = append(dbVariants, domain.ProductCatalog{
				MerchantID:     merchantID,
				ProductID:      strconv.FormatInt(product.ID, 10),
				VariantID:      strconv.FormatInt(v.ID, 10),
				Title:          fmt.Sprintf("%s - %s", product.Title, v.Title),
				SKU:            v.SKU,
				Category:       product.ProductType,
				Tags:           product.Tags,
				PricePaise:     pricePaise,
				CompareAtPaise: comparePaise,
				LastSyncedAt:   time.Now(),
			})
		}

		if len(dbVariants) > 0 {
			err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "variant_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"title", "sku", "category", "tags", "price_paise", "compare_at_paise", "last_synced_at"}),
			}).Create(&dbVariants).Error
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		slog.Error("failed to process product upsert webhook", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal storage error"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true})
}

// HandleProductDelete handles products/delete webhooks from Shopify.
func (h *CatalogWebhookHandler) HandleProductDelete(c *fiber.Ctx) error {
	merchantID := c.Query("merchant_id")
	if merchantID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing merchant_id"})
	}

	rawBody := c.Body()
	ctx := c.UserContext()

	err := h.verifyShopifySignature(ctx, c, rawBody, merchantID)
	if err != nil {
		slog.Warn("shopify product delete webhook signature failed", "error", err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized webhook source"})
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "failed to parse payload"})
	}

	// Soft-delete or hard-delete all variant catalog rows associated with the product ID
	productIDStr := strconv.FormatInt(payload.ID, 10)
	err = h.pg.WithContext(ctx).Where("merchant_id = ? AND product_id = ?", merchantID, productIDStr).Delete(&domain.ProductCatalog{}).Error
	if err != nil {
		slog.Error("failed to delete product variants from catalog", "product_id", productIDStr, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete catalog items"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true})
}

// verifyShopifySignature decrypts client secret and validates HMAC-SHA256 in constant time
func (h *CatalogWebhookHandler) verifyShopifySignature(ctx context.Context, c *fiber.Ctx, body []byte, merchantID string) error {
	shopifyHmac := c.Get("X-Shopify-Hmac-Sha256")
	if shopifyHmac == "" {
		return errors.New("missing X-Shopify-Hmac-Sha256 header")
	}

	// 1. Fetch merchant credentials for shopify
	var creds domain.PlatformCredential
	err := h.pg.WithContext(ctx).
		Where("merchant_id = ? AND platform = 'shopify' AND is_active = true", merchantID).
		First(&creds).Error
	if err != nil {
		return fmt.Errorf("failed to fetch shopify credentials: %w", err)
	}

	// 2. Decrypt secret key
	masterKeyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		return errors.New("TOKEN_ENCRYPTION_KEY is unset")
	}
	masterKeyBytes, err := base64.StdEncoding.DecodeString(masterKeyStr)
	if err != nil {
		return errors.New("invalid base64 token encryption key")
	}

	decrypted, err := security.DecryptString(creds.WebhookSecretEncrypted, masterKeyBytes)
	var secretBytes []byte
	if err == nil {
		secretBytes = []byte(decrypted)
	} else {
		secretBytes = []byte(creds.WebhookSecretEncrypted)
	}

	// 3. Cryptographically validate signature in constant time
	if !security.ValidateHMAC(body, shopifyHmac, secretBytes) {
		return errors.New("invalid signature HMAC")
	}
	return nil
}

func parsePriceToPaise(priceStr string) int {
	if priceStr == "" {
		return 0
	}
	var f float64
	_, err := fmt.Sscanf(priceStr, "%f", &f)
	if err != nil {
		return 0
	}
	return int(f * 100)
}
