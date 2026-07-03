package handlers

import (
	"log/slog"
	"os"

	"github.com/arpitmandhotra/api-integrator/internal/billing"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type BillingHandler struct {
	pg *gorm.DB
}

func NewBillingHandler(pgDB *gorm.DB) *BillingHandler {
	return &BillingHandler{
		pg: pgDB,
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

	// Flip IsActive to true in Postgres to upgrade from Shadow to Active Mode
	err := h.pg.WithContext(c.UserContext()).Model(&domain.Merchant{}).
		Where("id = ?", merchantID).
		Update("is_active", true).Error

	if err != nil {
		slog.Error("failed to activate merchant after successful payment", "merchant_id", merchantID, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to activate account",
		})
	}

	slog.Info("merchant activated successfully", "merchant_id", merchantID, "order_id", req.OrderID)
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Payment verified and account activated successfully",
	})
}
