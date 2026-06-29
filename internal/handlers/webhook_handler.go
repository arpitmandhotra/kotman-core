package handlers

import (
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type WebhookHandler struct {
	pg *gorm.DB 
}

func NewWebhookHandler(pgDB *gorm.DB) *WebhookHandler {
	return &WebhookHandler{pg: pgDB}
}

// ==========================================
// 1. SHOPIFY ADAPTER
// ==========================================
func (h *WebhookHandler) HandleShopify(c *fiber.Ctx) error {
	topic := c.Get("X-Shopify-Topic") 

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
	topic := c.Get("X-WC-Webhook-Topic") 

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
	eventTopic := c.Get("X-Magento-Event") 

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
	
	merchantID, ok := c.Locals("kotman.merchant_id").(string)
	if !ok {
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
	// SQL Injection Prevention Allowlist
	allowed := map[string]bool{
		"total_orders":          true,
		"successful_deliveries": true,
		"total_rtos":            true,
		"total_cancellations":   true,
	}
	if !allowed[columnName] {
		slog.Error("blocked invalid column name execution", "column", columnName)
		return
	}

	query := `
		INSERT INTO trust_profiles (phone_hash, ` + columnName + `, created_at, updated_at) 
		VALUES (?, 1, NOW(), NOW()) 
		ON CONFLICT (phone_hash) 
		DO UPDATE SET ` + columnName + ` = trust_profiles.` + columnName + ` + 1, updated_at = NOW();
	`
	
	result := h.pg.Exec(query, phoneHash)
	if result.Error != nil {
		slog.Error("failed to update ai metrics", "error", result.Error, "hash", phoneHash[:8])
		return
	}
	if result.RowsAffected == 0 {
		slog.Error("metric upsert affected zero rows — possible race condition",
			"hash", phoneHash[:8],
			"column", columnName,
		)
	}
}