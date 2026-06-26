package handlers

import (
	"log/slog"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// WebhookHandler acts as the Omni-Channel Adapter for multiple platforms
type WebhookHandler struct {
	pg *gorm.DB // We inject Postgres directly for high-speed metric updates
}

func NewWebhookHandler(pgDB *gorm.DB) *WebhookHandler {
	return &WebhookHandler{pg: pgDB}
}

// ==========================================
// 1. SHOPIFY ADAPTER
// ==========================================
func (h *WebhookHandler) HandleShopify(c *fiber.Ctx) error {
	// Shopify sends the event type in this header (e.g., "orders/create", "orders/fulfilled")
	topic := c.Get("X-Shopify-Topic") 

	var payload struct {
		Customer struct {
			Phone string `json:"phone"`
		} `json:"customer"`
	}

	if err := c.BodyParser(&payload); err != nil {
		slog.Error("shopify webhook invalid json", "error", err)
		return c.SendStatus(fiber.StatusOK) // ALWAYS return 200 to prevent infinite Shopify retries
	}

	if payload.Customer.Phone == "" {
		return c.SendStatus(fiber.StatusOK) // Ignore checkouts where the user didn't leave a phone number
	}

	// Hash AFTER parsing
	phoneHash := crypto.HashPhone(payload.Customer.Phone)

	// Route the event to the correct AI metric
	switch topic {
	case "orders/create":
		h.incrementMetric(phoneHash, "total_orders")
	case "orders/fulfilled":
		h.incrementMetric(phoneHash, "successful_deliveries")
	case "orders/cancelled", "refunds/create":
		h.incrementMetric(phoneHash, "total_rtos")
	}

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// 2. WOOCOMMERCE ADAPTER
// ==========================================
func (h *WebhookHandler) HandleWooCommerce(c *fiber.Ctx) error {
	// WooCommerce uses a different header and JSON structure
	topic := c.Get("X-WC-Webhook-Topic") // e.g., "order.created", "order.completed"

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

	switch topic {
	case "order.created":
		h.incrementMetric(phoneHash, "total_orders")
	case "order.completed":
		h.incrementMetric(phoneHash, "successful_deliveries")
	case "order.refunded", "order.cancelled":
		h.incrementMetric(phoneHash, "total_rtos")
	}

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// 3. MAGENTO ADAPTER
// ==========================================
func (h *WebhookHandler) HandleMagento(c *fiber.Ctx) error {
	// Magento extensions often send the event type in a custom header or inside the body
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

	// Map Magento statuses to our AI metrics
	switch eventTopic {
	case "sales_order_save_after":
		if payload.Order.Status == "pending" {
			h.incrementMetric(phoneHash, "total_orders")
		} else if payload.Order.Status == "complete" {
			h.incrementMetric(phoneHash, "successful_deliveries")
		} else if payload.Order.Status == "canceled" || payload.Order.Status == "closed" {
			h.incrementMetric(phoneHash, "total_rtos")
		}
	}

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// THE CORE AI ENGINE UPDATER (Universal Funnel)
// ==========================================
func (h *WebhookHandler) incrementMetric(phoneHash string, columnName string) {
	// This uses GORM's raw SQL execution for an "Atomic Upsert".
	// If the buyer doesn't exist yet, it creates them. 
	// If they do exist, it securely adds +1 to the correct column.
	query := `
		INSERT INTO trust_profiles (phone_hash, ` + columnName + `, created_at, updated_at) 
		VALUES (?, 1, NOW(), NOW()) 
		ON CONFLICT (phone_hash) 
		DO UPDATE SET ` + columnName + ` = trust_profiles.` + columnName + ` + 1, updated_at = NOW();
	`
	
	if err := h.pg.Exec(query, phoneHash).Error; err != nil {
		slog.Error("failed to update ai metrics", "error", err, "hash", phoneHash[:8])
	}
}
