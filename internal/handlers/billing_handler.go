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

// CreateWalletTopUp initializes a payment order in Razorpay for a wallet top-up
func (h *BillingHandler) CreateWalletTopUp(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kotman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"error":   "Unauthorized: merchant context missing",
		})
	}

	var req TopUpRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "Invalid request body",
		})
	}

	keyID := os.Getenv("RAZORPAY_KEY_ID")
	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if keyID == "" || keySecret == "" {
		slog.Error("Razorpay credentials are not set in the environment")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Billing system temporarily unavailable",
		})
	}

	amountPaise := int64(req.Amount * 100)
	if amountPaise <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "Invalid top-up amount",
		})
	}

	orderID, err := billing.CreateRazorpayOrder(amountPaise, keyID, keySecret)
	if err != nil {
		slog.Error("failed to create Razorpay order", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to initialize payment order",
		})
	}

	// After orderID is returned successfully, add:
	amountKey := "topup:amount:" + orderID
	h.rdb.Set(c.UserContext(), amountKey, amountPaise, 30*time.Minute)

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success":           true,
		"razorpay_order_id": orderID,
	})
}

// VerifyPaymentAndActivate validates Razorpay signatures and transitions the merchant to Active Mode
func (h *BillingHandler) VerifyPaymentAndActivate(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kotman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"error":   "Unauthorized: merchant context missing",
		})
	}

	var req VerifyRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "Invalid request body",
		})
	}

	keySecret := os.Getenv("RAZORPAY_KEY_SECRET")
	if keySecret == "" {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Billing system configuration error",
		})
	}

	// Verify payment cryptographically
	isValid := billing.VerifyRazorpaySignature(req.OrderID, req.PaymentID, req.Signature, keySecret)
	if !isValid {
		slog.Warn("invalid Razorpay signature received", "order_id", req.OrderID, "payment_id", req.PaymentID)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"error":   "Payment signature verification failed",
		})
	}

	ctx := c.UserContext()
	amountStr, err := h.rdb.Get(ctx, "topup:amount:"+req.OrderID).Result()
	var amountPaise int64
	if err == nil {
		amountPaise, _ = strconv.ParseInt(amountStr, 10, 64)
	}

	err = h.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"is_active":               true,
			"has_rto_engine":          true,
			"has_cross_network_intel": true,
		}
		if err := tx.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(updates).Error; err != nil {
			return err
		}
		if amountPaise > 0 {
			return tx.Model(&domain.MerchantSettings{}).
				Where("merchant_id = ?", merchantID).
				Update("wallet_balance_paise", gorm.Expr("wallet_balance_paise + ?", amountPaise)).Error
		}
		return nil
	})

	if err != nil {
		slog.Error("failed to activate merchant or credit wallet after successful payment", "merchant_id", merchantID, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to activate account",
		})
	}

	h.rdb.Del(ctx, "topup:amount:"+req.OrderID)

	slog.Info("merchant activated successfully", "merchant_id", merchantID, "order_id", req.OrderID)
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Payment verified and account activated successfully",
	})
}

// PurchaseModule initiates a Razorpay order for a flat-fee module subscription.
// Route: POST /v1/billing/module/purchase
// Body: { "module": "cross_network" | "crm_upsell" }
func (h *BillingHandler) PurchaseModule(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kotman.merchant_id")
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

	// Price is ₹4,999/month for both flat-fee modules
	const modulePriceINR = 4999
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
	merchantIDVal := c.Locals("kotman.merchant_id")
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

	note := ""
	if moduleName == domain.ModuleCrossNetwork && merchant.HasCrossNetworkIntel {
		note = "Module was already active; subscription renewed/re-verified"
	} else if moduleName == domain.ModuleCRMUpsell && merchant.HasCRMUpsellEngine {
		note = "Module was already active; subscription renewed/re-verified"
	}

	// Activate the module in a Postgres transaction
	now := time.Now()
	renewsAt := now.AddDate(0, 1, 0) // 1 month from now

	err = h.pg.WithContext(c.UserContext()).Transaction(func(tx *gorm.DB) error {
		// Upsert MerchantSubscription row
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

		// Flip the corresponding bool on Merchant
		updates := map[string]interface{}{}
		switch moduleName {
		case domain.ModuleCrossNetwork:
			updates["has_cross_network_intel"] = true
			updates["cross_network_renews_at"]  = renewsAt
		case domain.ModuleCRMUpsell:
			updates["has_crm_upsell_engine"] = true
			updates["crm_upsell_renews_at"]   = renewsAt
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

	slog.Info("module activated", "module", moduleName, "merchant_id", merchantID, "renews_at", renewsAt)
	
	resp := fiber.Map{
		"success":   true,
		"module":    moduleName,
		"renews_at": renewsAt,
		"message":   "Module activated successfully",
	}
	if note != "" {
		resp["note"] = note
	}
	return c.JSON(resp)
}
