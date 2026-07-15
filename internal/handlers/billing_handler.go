package handlers

import (
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

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// Upfront subscription payment of ₹4,999/month for flat-fee modules
	const modulePriceINR = 4999
	amountPaise := int64(modulePriceINR * 100)

	keyID := os.Getenv("RAZORPAY_KEY_ID")
	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if keyID == "" || keySecret == "" {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "billing unavailable"})
	}

	orderID, err := billing.CreateRazorpayOrder(amountPaise, keyID, keySecret)
	if err != nil {
		slog.Error("failed to create Razorpay order for module", "module", moduleName, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to create payment order"})
	}

	cacheKey := "module_purchase:" + orderID
	cacheVal := moduleName + ":" + strconv.Itoa(modulePriceINR)
	h.rdb.Set(c.UserContext(), cacheKey, cacheVal, 30*time.Minute)

	return c.Status(201).JSON(fiber.Map{
		"success":           true,
		"razorpay_order_id": orderID,
		"module":            moduleName,
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

	var req VerifyRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if !billing.VerifyRazorpaySignature(req.OrderID, req.PaymentID, req.Signature, keySecret) {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "signature verification failed"})
	}

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

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	now := time.Now()
	renewsAt := now.AddDate(0, 1, 0) // monthly renewal date

	err = h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		sub := domain.MerchantSubscription{
			MerchantID:          merchantID,
			Module:              moduleName,
			Status:              "active",
			PriceINR:            4999,
			RazorpayOrderID:     req.OrderID,
			CurrentPeriodStart:  &now,
			CurrentPeriodEnd:    &renewsAt,
		}
		if err := tx.Where("merchant_id = ? AND module = ?", merchantID, moduleName).
			Assign(sub).FirstOrCreate(&sub).Error; err != nil {
			return err
		}

		updates := map[string]interface{}{
			"has_paid_subscription":       true,
			"paid_subscription_renews_at": renewsAt,
		}
		return tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error
	})

	if err != nil {
		slog.Error("failed to activate module", "module", moduleName, "merchant_id", merchantID, "error", err)
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to activate module"})
	}

	h.rdb.Del(c.UserContext(), cacheKey)

	slog.Info("module purchased and activated in real time", "module", moduleName, "merchant_id", merchantID)
	
	return c.JSON(fiber.Map{
		"success": true,
		"module":  moduleName,
		"message": "Module purchased and activated successfully",
	})
}
