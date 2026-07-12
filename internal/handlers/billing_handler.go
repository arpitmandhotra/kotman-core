package handlers

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/billing"
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
			"is_active":               true,
			"has_rto_engine":          true,
			"has_cross_network_intel": true,
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

	// Validate module name
	if req.Module != domain.ModuleCrossNetwork && req.Module != domain.ModuleCRMUpsell {
		return c.Status(400).JSON(fiber.Map{
			"success": false,
			"error":   "invalid module. must be 'cross_network' or 'crm_upsell'",
		})
	}

	// Check if RTO engine is active — cross_network is free with it
	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}
	if req.Module == domain.ModuleCrossNetwork && merchant.HasRTOEngine {
		return c.Status(400).JSON(fiber.Map{
			"success": false,
			"error":   "cross_network intelligence is already included with your RTO Engine subscription",
		})
	}

	// One-time payment of ₹5,000 for flat-fee modules
	const modulePriceINR = 5000
	amountPaise := int64(modulePriceINR * 100)

	keyID := os.Getenv("RAZORPAY_KEY_ID")
	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if keyID == "" || keySecret == "" {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "billing unavailable"})
	}

	orderID, err := billing.CreateRazorpayOrder(amountPaise, keyID, keySecret)
	if err != nil {
		slog.Error("failed to create Razorpay order for module", "module", req.Module, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to create payment order"})
	}

	// Store the (module, amount) in Redis keyed on Razorpay order ID for the verify step
	cacheKey := "module_purchase:" + orderID
	cacheVal := req.Module + ":" + strconv.Itoa(modulePriceINR)
	h.rdb.Set(c.UserContext(), cacheKey, cacheVal, 30*time.Minute)

	return c.Status(201).JSON(fiber.Map{
		"success":           true,
		"razorpay_order_id": orderID,
		"module":            req.Module,
		"amount_inr":        modulePriceINR,
	})
}

// VerifyModulePurchase validates payment and activates the purchased module.
// Route: POST /v1/billing/module/verify
// Body: { "razorpay_order_id", "razorpay_payment_id", "razorpay_signature" }
func (h *BillingHandler) VerifyModulePurchase(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var req VerifyRequest // reuse existing struct
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if !billing.VerifyRazorpaySignature(req.OrderID, req.PaymentID, req.Signature, keySecret) {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "signature verification failed"})
	}

	// Retrieve module from Redis cache
	cacheKey := "module_purchase:" + req.OrderID
	cacheVal, err := h.rdb.Get(c.UserContext(), cacheKey).Result()
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "payment session expired or not found"})
	}

	parts := strings.SplitN(cacheVal, ":", 2)
	if len(parts) != 2 {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "invalid session data"})
	}
	moduleName := parts[0]

	// Fetch current merchant state for idempotency check
	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// Activate the module in a Postgres transaction (One-time lifetime purchase, no renews-at date needed)
	now := time.Now()
	err = h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		// Upsert MerchantSubscription row
		sub := domain.MerchantSubscription{
			MerchantID:          merchantID,
			Module:              moduleName,
			Status:              "active",
			PriceINR:            5000,
			RazorpayOrderID:     req.OrderID,
			CurrentPeriodStart:  &now,
			CurrentPeriodEnd:    nil, // one-time payment, no end date
		}
		if err := tx.Where("merchant_id = ? AND module = ?", merchantID, moduleName).
			Assign(sub).FirstOrCreate(&sub).Error; err != nil {
			return err
		}

		// Flip the corresponding bool on Merchant
		updates := map[string]interface{}{}
		switch moduleName {
		case domain.ModuleCrossNetwork:
			updates["has_cross_network_intel"] = true
			updates["cross_network_renews_at"]  = nil
		case domain.ModuleCRMUpsell:
			updates["has_crm_upsell_engine"] = true
			updates["crm_upsell_renews_at"]   = nil
		default:
			return fmt.Errorf("unknown module: %s", moduleName)
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to activate module", "module", moduleName, "merchant_id", merchantID, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to activate module"})
	}

	// Clean up Redis cache key
	h.rdb.Del(c.UserContext(), cacheKey)

	slog.Info("module purchased and activated in real time", "module", moduleName, "merchant_id", merchantID)
	
	return c.JSON(fiber.Map{
		"success": true,
		"module":  moduleName,
		"message": "Module purchased and activated successfully",
	})
}
