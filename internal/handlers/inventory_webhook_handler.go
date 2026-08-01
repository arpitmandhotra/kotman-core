package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/inventory"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// InventoryWebhookHandler processes inbound inventory events from Shopify,
// WooCommerce, and Magento. It is intentionally separate from the main
// WebhookHandler to keep the order-intelligence surface and the inventory
// intelligence surface cleanly decoupled.
type InventoryWebhookHandler struct {
	pg          *gorm.DB
	snapshotSvc *inventory.SnapshotService
}

// NewInventoryWebhookHandler wires up the inventory webhook handler.
func NewInventoryWebhookHandler(pg *gorm.DB) *InventoryWebhookHandler {
	return &InventoryWebhookHandler{
		pg:          pg,
		snapshotSvc: inventory.NewSnapshotService(pg),
	}
}

// ==========================================
// SHOPIFY — inventory_levels/update
//           inventory_items/update
// ==========================================

// HandleShopifyInventory handles both Shopify inventory webhook topics:
//   - inventory_levels/update  → quantity at a location changed
//   - inventory_items/update   → item metadata (SKU) changed
//
// The topic is read from the X-Shopify-Topic header, which is already
// verified upstream by the RequireShopifyHMAC middleware.
func (h *InventoryWebhookHandler) HandleShopifyInventory(c *fiber.Ctx) error {
	rawBody := c.Body()
	topic := c.Get("X-Shopify-Topic")

	merchantID := h.resolveMerchantID(c, "shopify")
	if merchantID == "" {
		slog.Warn("inventory webhook: could not resolve merchant for shopify inventory event",
			"topic", topic, "ip", c.IP())
		return c.SendStatus(fiber.StatusOK)
	}

	// Copy body for safe goroutine use (Fiber recycles the underlying buffer)
	bodyCopy := make([]byte, len(rawBody))
	copy(bodyCopy, rawBody)

	go func(mID, top string, body []byte) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("inventory webhook: panic in shopify goroutine", "panic", r, "merchant_id", mID)
			}
		}()
		h.snapshotSvc.UpsertSnapshotFromShopify(c.UserContext(), mID, top, body)
	}(merchantID, topic, bodyCopy)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// WOOCOMMERCE — woocommerce_product_object_updated
// ==========================================

// HandleWooCommerceInventory handles the woocommerce_product_object_updated action.
// The action name is present in the X-WC-Webhook-Topic header.
func (h *InventoryWebhookHandler) HandleWooCommerceInventory(c *fiber.Ctx) error {
	rawBody := c.Body()
	topic := c.Get("X-WC-Webhook-Topic")

	if topic != domain.WooTopicProductUpdated {
		// Different product topic (e.g. product.created) — not relevant for inventory snapshots
		return c.SendStatus(fiber.StatusOK)
	}

	merchantID := h.resolveMerchantID(c, "woocommerce")
	if merchantID == "" {
		slog.Warn("inventory webhook: could not resolve merchant for woocommerce product update", "ip", c.IP())
		return c.SendStatus(fiber.StatusOK)
	}

	bodyCopy := make([]byte, len(rawBody))
	copy(bodyCopy, rawBody)

	go func(mID string, body []byte) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("inventory webhook: panic in woocommerce goroutine", "panic", r, "merchant_id", mID)
			}
		}()
		h.snapshotSvc.UpsertSnapshotFromWooCommerce(c.UserContext(), mID, body)
	}(merchantID, bodyCopy)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// MAGENTO — catalog_product_save_after
// ==========================================

// HandleMagentoInventory handles the catalog_product_save_after Magento Observer event.
// The event name is present in the X-Magento-Event header.
func (h *InventoryWebhookHandler) HandleMagentoInventory(c *fiber.Ctx) error {
	rawBody := c.Body()
	eventName := c.Get("X-Magento-Event")

	if eventName != domain.MagentoEventCatalogProductSaveAfter {
		// Different Magento event — not relevant for inventory snapshots
		return c.SendStatus(fiber.StatusOK)
	}

	merchantID := h.resolveMerchantID(c, "magento")
	if merchantID == "" {
		slog.Warn("inventory webhook: could not resolve merchant for magento product save", "ip", c.IP())
		return c.SendStatus(fiber.StatusOK)
	}

	bodyCopy := make([]byte, len(rawBody))
	copy(bodyCopy, rawBody)

	go func(mID string, body []byte) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("inventory webhook: panic in magento goroutine", "panic", r, "merchant_id", mID)
			}
		}()
		h.snapshotSvc.UpsertSnapshotFromMagento(c.UserContext(), mID, body)
	}(merchantID, bodyCopy)

	return c.SendStatus(fiber.StatusOK)
}

// ==========================================
// SHARED HELPERS
// ==========================================

// resolveMerchantID is a local duplicate of WebhookHandler.resolveMerchantID
// to keep InventoryWebhookHandler self-contained without importing the parent.
func (h *InventoryWebhookHandler) resolveMerchantID(c *fiber.Ctx, platform string) string {
	var shopDomain string
	switch platform {
	case "shopify":
		shopDomain = c.Get("X-Shopify-Shop-Domain")
	case "woocommerce":
		shopDomain = c.Get("X-Wc-Webhook-Source")
		if shopDomain == "" {
			return ""
		}
	case "magento":
		shopDomain = c.Get("X-Kaughtman-Merchant-Domain")
	}

	if shopDomain == "" {
		return ""
	}

	cleaned := cleanDomainStr(shopDomain)
	escapedForLike := escapeForLike(cleaned)

	var cred domain.PlatformCredential
	err := h.pg.Where(
		"platform = ? AND is_active = ? AND (LOWER(shop_domain) = ? OR LOWER(shop_domain) LIKE ? ESCAPE '\\\\')",
		platform, true, cleaned, "%"+escapedForLike+"%",
	).First(&cred).Error
	if err == nil {
		return cred.MerchantID
	}
	return ""
}

// cleanDomainStr strips protocol prefix/suffix from a shop domain.
func cleanDomainStr(d string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(d) > len(prefix) && d[:len(prefix)] == prefix {
			d = d[len(prefix):]
			break
		}
	}
	if len(d) > 0 && d[len(d)-1] == '/' {
		d = d[:len(d)-1]
	}
	return toLowerTrimmed(d)
}

// escapeForLike escapes LIKE metacharacters to prevent SQL injection via
// wildcard expansion in the shop_domain LIKE clause.
func escapeForLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%':
			out = append(out, '\\', '%')
		case '_':
			out = append(out, '\\', '_')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// toLowerTrimmed returns a lowercase trimmed version of s using pure byte ops
// (avoids importing strings just for this).
func toLowerTrimmed(s string) string {
	result := make([]byte, len(s))
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result[i-start] = c
	}
	return string(result[:end-start])
}

// parseInventoryBody is a utility used in tests to inspect the raw payload
// without triggering DB side effects.
func parseInventoryBody(raw []byte) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
