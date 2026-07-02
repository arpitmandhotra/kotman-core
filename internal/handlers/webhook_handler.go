package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/billing"
	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type WebhookHandler struct {
	pg            *gorm.DB
	shopifySecret string
	wooSecret     string
	magentoSecret string
}

func NewWebhookHandler(pgDB *gorm.DB, shopifySecret, wooSecret, magentoSecret string) *WebhookHandler {
	return &WebhookHandler{
		pg:            pgDB,
		shopifySecret: shopifySecret,
		wooSecret:     wooSecret,
		magentoSecret: magentoSecret,
	}
}

// resolveMerchantID locates the Merchant ID associated with the incoming webhook platform request
func (h *WebhookHandler) resolveMerchantID(c *fiber.Ctx, platform string) string {
	var shopDomain string
	switch platform {
	case "shopify":
		shopDomain = c.Get("X-Shopify-Shop-Domain")
	case "woocommerce":
		shopDomain = c.Get("X-Wc-Webhook-Source")
		if shopDomain == "" {
			shopDomain = c.BaseURL()
		}
	case "magento":
		shopDomain = c.BaseURL()
	}

	if shopDomain == "" {
		return ""
	}

	cleanDomain := func(d string) string {
		d = strings.TrimPrefix(d, "https://")
		d = strings.TrimPrefix(d, "http://")
		d = strings.TrimSuffix(d, "/")
		return strings.ToLower(strings.TrimSpace(d))
	}

	cleaned := cleanDomain(shopDomain)

	var cred domain.PlatformCredential
	err := h.pg.Where("platform = ? AND is_active = ? AND (LOWER(shop_domain) = ? OR LOWER(shop_domain) LIKE ?)",
		platform, true, cleaned, "%"+cleaned+"%").First(&cred).Error
	if err == nil {
		return cred.MerchantID
	}

	return ""
}

// ==========================================
// 1. SHOPIFY ADAPTER
// ==========================================
func (h *WebhookHandler) HandleShopify(c *fiber.Ctx) error {
	rawBody := c.Body()

	// 1. Webhook signature validation BEFORE parsing or billing
	shopifySignature := c.Get("X-Shopify-Hmac-Sha256")
	if shopifySignature == "" {
		slog.Warn("shopify webhook blocked: missing signature")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	mac := hmac.New(sha256.New, []byte(h.shopifySecret))
	mac.Write(rawBody)
	expectedMAC := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(shopifySignature), []byte(expectedMAC)) != 1 {
		slog.Warn("shopify webhook blocked: cryptographic mismatch")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	topic := c.Get("X-Shopify-Topic")

	// BILLING: process this order for fee calculation
	merchantID := h.resolveMerchantID(c, "shopify")
	if merchantID != "" {
		bodyCopy := make([]byte, len(rawBody))
		copy(bodyCopy, rawBody)
		go func(platform, mID string, body []byte) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := billing.ProcessInboundOrder(ctx, platform, mID, body); err != nil {
				slog.Error("billing ingestion failed",
					"platform", platform,
					"merchant_id", mID,
					"error", err,
				)
			}
		}("shopify", merchantID, bodyCopy)
	} else {
		slog.Warn("could not resolve merchant ID for shopify billing webhook")
	}

	var payload struct {
		Customer struct {
			Phone string `json:"phone"`
		} `json:"customer"`
	}

	if err := c.BodyParser(&payload); err != nil {
		slog.Error("shopify webhook invalid json", "error", err)
		return c.SendStatus(fiber.StatusOK)
	}

	if payload.Customer.Phone == "" {
		return c.SendStatus(fiber.StatusOK)
	}

	phoneHash := crypto.HashPhone(payload.Customer.Phone)

	// Offload synchronous DB execution to a background goroutine
	go func(topic, hash string) {
		switch topic {
		case "orders/create":
			h.incrementMetric(hash, "total_orders")
		case "orders/fulfilled":
			h.incrementMetric(hash, "successful_deliveries")
		case "orders/cancelled", "refunds/create":
			h.incrementMetric(hash, "total_rtos")
		}
	}(topic, phoneHash)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// 2. WOOCOMMERCE ADAPTER
// ==========================================
func (h *WebhookHandler) HandleWooCommerce(c *fiber.Ctx) error {
	rawBody := c.Body()

	// 1. Webhook signature validation BEFORE parsing or billing
	wooSignature := c.Get("X-WC-Webhook-Signature")
	if wooSignature == "" {
		slog.Warn("woocommerce webhook blocked: missing signature")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	mac := hmac.New(sha256.New, []byte(h.wooSecret))
	mac.Write(rawBody)
	expectedMAC := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(wooSignature), []byte(expectedMAC)) != 1 {
		slog.Warn("woocommerce webhook blocked: cryptographic mismatch")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	topic := c.Get("X-WC-Webhook-Topic")

	// BILLING: process this order for fee calculation
	merchantID := h.resolveMerchantID(c, "woocommerce")
	if merchantID != "" {
		bodyCopy := make([]byte, len(rawBody))
		copy(bodyCopy, rawBody)
		go func(platform, mID string, body []byte) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := billing.ProcessInboundOrder(ctx, platform, mID, body); err != nil {
				slog.Error("billing ingestion failed",
					"platform", platform,
					"merchant_id", mID,
					"error", err,
				)
			}
		}("woocommerce", merchantID, bodyCopy)
	} else {
		slog.Warn("could not resolve merchant ID for woocommerce billing webhook")
	}

	var payload struct {
		Billing struct {
			Phone string `json:"phone"`
		} `json:"billing"`
	}

	if err := c.BodyParser(&payload); err != nil {
		return c.SendStatus(fiber.StatusOK)
	}

	if payload.Billing.Phone == "" {
		return c.SendStatus(fiber.StatusOK)
	}

	phoneHash := crypto.HashPhone(payload.Billing.Phone)

	// Offload synchronous DB execution to a background goroutine
	go func(topic, hash string) {
		switch topic {
		case "order.created":
			h.incrementMetric(hash, "total_orders")
		case "order.completed":
			h.incrementMetric(hash, "successful_deliveries")
		case "order.refunded", "order.cancelled":
			h.incrementMetric(hash, "total_rtos")
		}
	}(topic, phoneHash)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// 3. MAGENTO ADAPTER
// ==========================================
func (h *WebhookHandler) HandleMagento(c *fiber.Ctx) error {
	rawBody := c.Body()

	// 1. Webhook signature validation BEFORE parsing or billing
	authHeader := c.Get("Authorization")
	expectedHeader := "Bearer " + h.magentoSecret

	if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedHeader)) != 1 {
		slog.Warn("magento webhook blocked: unauthorized")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	eventTopic := c.Get("X-Magento-Event")

	// BILLING: process this order for fee calculation
	merchantID := h.resolveMerchantID(c, "magento")
	if merchantID != "" {
		bodyCopy := make([]byte, len(rawBody))
		copy(bodyCopy, rawBody)
		go func(platform, mID string, body []byte) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := billing.ProcessInboundOrder(ctx, platform, mID, body); err != nil {
				slog.Error("billing ingestion failed",
					"platform", platform,
					"merchant_id", mID,
					"error", err,
				)
			}
		}("magento", merchantID, bodyCopy)
	} else {
		slog.Warn("could not resolve merchant ID for magento billing webhook")
	}

	var payload struct {
		Order struct {
			BillingAddress struct {
				Telephone string `json:"telephone"`
			} `json:"billing_address"`
			Status string `json:"status"`
		} `json:"order"`
	}

	if err := c.BodyParser(&payload); err != nil {
		return c.SendStatus(fiber.StatusOK)
	}

	phone := payload.Order.BillingAddress.Telephone
	if phone == "" {
		return c.SendStatus(fiber.StatusOK)
	}

	phoneHash := crypto.HashPhone(phone)

	// Offload synchronous DB execution to a background goroutine
	go func(topic, status, hash string) {
		switch topic {
		case "sales_order_save_after":
			if status == "pending" {
				h.incrementMetric(hash, "total_orders")
			} else if status == "complete" {
				h.incrementMetric(hash, "successful_deliveries")
			} else if status == "canceled" || status == "closed" {
				h.incrementMetric(hash, "total_rtos")
			}
		}
	}(eventTopic, payload.Order.Status, phoneHash)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// 4. INTENT-WEIGHTED FEEDBACK INGESTION
// ==========================================
func (h *WebhookHandler) HandleProductReview(c *fiber.Ctx) error {
	rawBody := c.Body()

	// 1. Webhook signature validation BEFORE parsing or billing
	shopifySignature := c.Get("X-Shopify-Hmac-Sha256")
	if shopifySignature == "" {
		slog.Warn("shopify review webhook blocked: missing signature")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	mac := hmac.New(sha256.New, []byte(h.shopifySecret))
	mac.Write(rawBody)
	expectedMAC := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(shopifySignature), []byte(expectedMAC)) != 1 {
		slog.Warn("shopify review webhook blocked: cryptographic mismatch")
		return c.SendStatus(fiber.StatusUnauthorized)
	}

	var payload struct {
		Customer struct {
			Phone string `json:"phone"`
		} `json:"customer"`
		OrderID  string  `json:"order_id"`
		SKU      string  `json:"sku"`
		Rating   float64 `json:"rating"` 
		Body     string  `json:"body"`
		Category string  `json:"category"` 
	}

	if err := c.BodyParser(&payload); err != nil {
		slog.Error("failed parsing inbound review packet", "error", err)
		return c.SendStatus(fiber.StatusOK)
	}

	if payload.Customer.Phone == "" {
		return c.SendStatus(fiber.StatusOK)
	}

	phoneHash := crypto.HashPhone(payload.Customer.Phone)
	
	merchantID := h.resolveMerchantID(c, "shopify")
	if merchantID == "" {
		slog.Error("failed extraction of validated merchant context")
		return c.SendStatus(fiber.StatusOK)
	}

	sentiment := 0.0
	if payload.Rating <= 2 {
		sentiment = -1.0
	} else if payload.Rating == 3 {
		sentiment = 0.0
	} else {
		sentiment = 1.0
	}

	// Hand execution off to background goroutine and return 200 OK instantly
	go h.processFeedback(phoneHash, merchantID, payload.OrderID, payload.SKU, payload.Category, sentiment)

	return c.SendStatus(fiber.StatusOK)
}

func (h *WebhookHandler) processFeedback(phoneHash, merchantID, orderID, sku, category string, sentiment float64) {
	now := time.Now()
	
	// 1. Save the raw feedback record
	feedback := domain.CustomerFeedback{
		PhoneHash:  phoneHash,
		MerchantID: merchantID,
		OrderID:    orderID,
		Category:   category,
		Sentiment:  sentiment,
		SKU:        sku,
		ReceivedAt: now,
	}

	if err := h.pg.Create(&feedback).Error; err != nil {
		slog.Error("failed recording raw customer feedback record", "error", err)
		return
	}

	// 2. Fetch the profile
	var profile domain.TrustProfile
	err := h.pg.FirstOrCreate(&profile, domain.TrustProfile{PhoneHash: phoneHash}).Error
	if err != nil {
		slog.Error("unable to resolve user trust record tracking target", "error", err)
		return
	}

	// 3. Determine the weight
	weightData, exists := domain.FeedbackWeights[category]
	if !exists {
		weightData = domain.FeedbackWeight{BuyerRiskDelta: -1.0, MerchantSignal: false, ProductSignal: false} 
	}

	// 4. Calculate the new Welford mean score locally first
	// We add +1 to the denominator here because we haven't updated the database count yet
	newComplaintScore := profile.ComplaintScore + ((sentiment - profile.ComplaintScore) / float64(profile.ComplaintCount+1))

	// 5. THE FIX: Execute an atomic, targeted update to prevent race conditions
	updatePayload := map[string]interface{}{
		"complaint_count":   gorm.Expr("complaint_count + 1"),
		"risk_adjustment":   gorm.Expr("risk_adjustment + ?", weightData.BuyerRiskDelta),
		"complaint_score":   newComplaintScore,
		"last_complaint_at": now,
	}

	result := h.pg.Model(&profile).Updates(updatePayload)
	if result.Error != nil {
		slog.Error("failed atomic update to merchant behavior profiling metrics", "error", result.Error)
		return
	}
	if result.RowsAffected == 0 {
		slog.Error("feedback profile update affected zero rows — risk adjustment lost",
			"hash", phoneHash[:8],
			"profile_id", profile.ID,
		)
		return
	}

	slog.Info("successfully processed intent-weighted customer feedback event", "hash", phoneHash[:8], "delta", weightData.BuyerRiskDelta)
}

// ==========================================
// CORE METRIC ENGINE
// ==========================================
func (h *WebhookHandler) incrementMetric(phoneHash string, columnName string) {
	database.IncrementMetric(h.pg, phoneHash, columnName)
}

// HandleShopifyUninstall sets the credential to inactive and deactivates the merchant key.
func (h *WebhookHandler) HandleShopifyUninstall(c *fiber.Ctx) error {
	shopDomain := c.Get("X-Shopify-Shop-Domain")
	if shopDomain == "" {
		var payload struct {
			Domain string `json:"domain"`
		}
		if err := c.BodyParser(&payload); err == nil && payload.Domain != "" {
			shopDomain = payload.Domain
		}
	}

	if shopDomain == "" {
		slog.Warn("shopify uninstall webhook received without shop domain")
		return c.SendStatus(fiber.StatusOK)
	}

	slog.Info("processing shopify app uninstall webhook", "shop", shopDomain)

	now := time.Now()
	err := h.pg.Transaction(func(tx *gorm.DB) error {
		var cred domain.PlatformCredential
		if err := tx.Where("platform = ? AND shop_domain = ? AND is_active = ?", "shopify", shopDomain, true).First(&cred).Error; err != nil {
			return err
		}

		cred.IsActive = false
		cred.UninstalledAt = &now
		if err := tx.Save(&cred).Error; err != nil {
			return err
		}

		var merchant domain.Merchant
		if err := tx.Where("id = ?", cred.MerchantID).First(&merchant).Error; err != nil {
			return err
		}
		merchant.IsActive = false
		if err := tx.Save(&merchant).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		slog.Error("failed processing shopify app uninstall transaction", "shop", shopDomain, "error", err)
	}

	return c.SendStatus(fiber.StatusOK)
}

func (h *WebhookHandler) HandleShopifyCustomersDataRequest(c *fiber.Ctx) error {
	return c.SendStatus(fiber.StatusOK)
}

func (h *WebhookHandler) HandleShopifyCustomersRedact(c *fiber.Ctx) error {
	return c.SendStatus(fiber.StatusOK)
}

func (h *WebhookHandler) HandleShopifyShopRedact(c *fiber.Ctx) error {
	var payload struct {
		ShopDomain string `json:"shop_domain"`
	}
	if err := c.BodyParser(&payload); err != nil {
		return c.SendStatus(fiber.StatusOK)
	}

	slog.Info("processing shopify GDPR shop redact webhook", "shop", payload.ShopDomain)

	now := time.Now()
	err := h.pg.Transaction(func(tx *gorm.DB) error {
		var cred domain.PlatformCredential
		if err := tx.Where("platform = ? AND shop_domain = ?", "shopify", payload.ShopDomain).First(&cred).Error; err != nil {
			return err
		}

		cred.IsActive = false
		if cred.UninstalledAt == nil {
			cred.UninstalledAt = &now
		}
		if err := tx.Save(&cred).Error; err != nil {
			return err
		}

		var merchant domain.Merchant
		if err := tx.Where("id = ?", cred.MerchantID).First(&merchant).Error; err != nil {
			return err
		}
		merchant.IsActive = false
		if err := tx.Save(&merchant).Error; err != nil {
			return err
		}

		if err := tx.Where("merchant_id = ?", merchant.ID).Delete(&domain.CustomerFeedback{}).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		slog.Error("failed shopify GDPR redact transaction", "shop", payload.ShopDomain, "error", err)
	}

	return c.SendStatus(fiber.StatusOK)
}