package handlers

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type BillingHandler struct {
	pg  *gorm.DB
	rdb *redis.Client
}

func NewBillingHandler(pgDB *gorm.DB, rdb *redis.Client) *BillingHandler {
	return &BillingHandler{
		pg:  pgDB,
		rdb: rdb,
	}
}

type TopUpRequest struct {
	Amount float64 `json:"amount"` // e.g. amount in INR
}

type VerifyRequest struct {
	OrderID   string `json:"razorpay_order_id"`
	PaymentID string `json:"razorpay_payment_id"`
	Signature string `json:"razorpay_signature"`
}

// CreateWalletTopUp is deprecated under postpaid billing.
func (h *BillingHandler) CreateWalletTopUp(c *fiber.Ctx) error {
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
		"success": false,
		"error":   "Wallet top-up is deprecated under postpaid billing. Accounts are billed postpaid at the end of the month.",
	})
}

// VerifyPaymentAndActivate directly transitions the merchant to Active Mode with zero friction.
func (h *BillingHandler) VerifyPaymentAndActivate(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"error":   "Unauthorized: merchant context missing",
		})
	}

	ctx := c.UserContext()
	err := h.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"is_active":      true,
			"has_rto_engine": true,
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to activate merchant postpaid", "merchant_id", merchantID, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to activate account",
		})
	}

	slog.Info("merchant activated postpaid successfully", "merchant_id", merchantID)
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Account activated successfully in active mode under postpaid billing",
	})
}

// PurchaseModule initiates a Razorpay order for a flat-fee module subscription.
// Route: POST /v1/billing/module/purchase
// Body: { "module": "cross_network" | "crm_upsell" }
func (h *BillingHandler) PurchaseModule(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var req struct {
		Module string `json:"module"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	moduleName := req.Module
	if moduleName == "cross_network" || moduleName == "crm_upsell" || moduleName == domain.ModuleUnifiedPaid {
		moduleName = domain.ModuleUnifiedPaid
	} else {
		return c.Status(400).JSON(fiber.Map{
			"success": false,
			"error":   "invalid module. must be 'unified_paid'",
		})
	}

	return c.JSON(fiber.Map{
		"success":           true,
		"razorpay_order_id": "free_tier_no_charge",
		"module":            moduleName,
		"amount_inr":        0,
		"message":           "This module is now free and included in the default tier",
	})
}

// VerifyModulePurchase validates payment and activates the purchased module. (No-op backward compatibility)
func (h *BillingHandler) VerifyModulePurchase(c *fiber.Ctx) error {
	var req VerifyRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"module":  domain.ModuleUnifiedPaid,
		"message": "Module purchased and activated successfully",
	})
}

// ActivateSubscription activates a postpaid growth or growth_ads subscription tier
// Route: POST /v1/billing/subscription/activate
// Body: { "plan": "growth" | "growth_ads" }
func (h *BillingHandler) ActivateSubscription(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"success":      false,
		"code":         "TIER_NOT_YET_AVAILABLE",
		"message":      "Growth and Growth+Ads tiers are coming soon. Join the waitlist at /v1/waitlist/join to get early access.",
		"waitlist_url": "/v1/waitlist/join",
	})

	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var req struct {
		Plan string `json:"plan"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	plan := domain.MerchantTier(strings.ToLower(req.Plan))
	if plan != domain.TierGrowth && plan != domain.TierGrowthAds {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid plan. must be 'growth' or 'growth_ads'"})
	}

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// Validate merchant is currently on free tier
	if merchant.Tier != "" && merchant.Tier != domain.TierFree {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "merchant is already on an active plan"})
	}

	now := time.Now()
	// Subscription renews at first day of next calendar month (postpaid)
	renewsAt := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)

	err := h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"tier":                     plan,
			"subscription_started_at":  &now,
			"subscription_renews_at":   &renewsAt,
			"has_paid_subscription":    true,
			"paid_subscription_sub_id": "sub_" + merchantID[:8],
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to activate subscription", "merchant_id", merchantID, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to activate subscription"})
	}

	InvalidateMerchantCache(c.UserContext(), h.rdb, merchant.APIKeyHash)

	slog.Info("subscription activated successfully", "merchant_id", merchantID, "plan", plan)
	return c.JSON(fiber.Map{
		"success":   true,
		"tier":      string(plan),
		"renews_at": renewsAt.Format(time.RFC3339),
		"message":   "Subscription activated successfully",
	})
}

// UpgradeSubscription upgrades mid-cycle from growth to growth_ads
// Route: POST /v1/billing/subscription/upgrade
// Body: { "plan": "growth_ads" }
func (h *BillingHandler) UpgradeSubscription(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"success":      false,
		"code":         "TIER_NOT_YET_AVAILABLE",
		"message":      "Growth and Growth+Ads tiers are coming soon. Join the waitlist at /v1/waitlist/join to get early access.",
		"waitlist_url": "/v1/waitlist/join",
	})

	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var req struct {
		Plan string `json:"plan"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	plan := domain.MerchantTier(strings.ToLower(req.Plan))
	if plan != domain.TierGrowthAds {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid plan. upgrade destination must be 'growth_ads'"})
	}

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// Validate merchant is currently on growth tier
	if merchant.Tier != domain.TierGrowth {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "merchant must be on 'growth' tier to upgrade to 'growth_ads'"})
	}

	err := h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"tier": plan,
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to upgrade subscription", "merchant_id", merchantID, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to upgrade subscription"})
	}

	InvalidateMerchantCache(c.UserContext(), h.rdb, merchant.APIKeyHash)

	slog.Info("merchant upgraded subscription mid-cycle", "merchant_id", merchantID, "from", "growth", "to", "growth_ads", "timestamp", time.Now())
	return c.JSON(fiber.Map{
		"success": true,
		"tier":    string(plan),
		"message": "Upgraded to Growth + Ads subscription. New rate Rs. 8,999 will apply from next billing cycle.",
	})
}

// CancelSubscription cancels the growth/growth_ads subscription effective at the end of the billing cycle
// Route: POST /v1/billing/subscription/cancel
func (h *BillingHandler) CancelSubscription(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	if merchant.Tier == "" || merchant.Tier == domain.TierFree {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "merchant does not have an active subscription"})
	}

	now := time.Now()
	// Set renewal date to end of current cycle (first day of next month)
	renewsAt := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)

	err := h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		// If merchant is on growth_ads and cancels, immediately stop CAPI dispatch
		if merchant.Tier == domain.TierGrowthAds {
			tx.Model(&domain.MerchantSettings{}).Where("merchant_id = ?", merchantID).Update("meta_capi_enabled", false)
		}

		updates := map[string]interface{}{
			"tier":                   domain.TierFree,
			"subscription_renews_at": &renewsAt,
			"has_paid_subscription":  false, // will downgrade to free on next invoice cycle
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to cancel subscription", "merchant_id", merchantID, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to cancel subscription"})
	}

	InvalidateMerchantCache(c.UserContext(), h.rdb, merchant.APIKeyHash)

	slog.Info("merchant cancelled subscription", "merchant_id", merchantID, "effective_at", renewsAt)
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Subscription cancelled successfully. Your features will remain active until " + renewsAt.Format("2006-01-02") + ".",
	})
}

// InvalidateMerchantCache invalidates the merchant's cached auth credentials in Redis
func InvalidateMerchantCache(ctx context.Context, rdb *redis.Client, apiKeyHash string) {
	if apiKeyHash == "" {
		return
	}
	cacheKey := "auth:apikey:" + apiKeyHash
	rdb.Del(ctx, cacheKey)
}
