package handlers

import (
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type AnalyticsHandler struct {
	pg *gorm.DB
}

func NewAnalyticsHandler(pgDB *gorm.DB) *AnalyticsHandler {
	return &AnalyticsHandler{
		pg: pgDB,
	}
}

// GetMerchantInsights aggregates shadow mode order audits for the merchant dashboard
func (h *AnalyticsHandler) GetMerchantInsights(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kotman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"error":   "Unauthorized: merchant context missing",
		})
	}

	var merchant domain.Merchant
	if err := h.pg.WithContext(c.UserContext()).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"error":   "Merchant not found",
		})
	}

	var totalOrdersAnalyzed int64
	if err := h.pg.WithContext(c.UserContext()).Model(&domain.OrderAudit{}).
		Where("merchant_id = ?", merchantID).
		Count(&totalOrdersAnalyzed).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to count analyzed orders",
		})
	}

	var highRiskOrdersFlagged int64
	if err := h.pg.WithContext(c.UserContext()).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND predicted_risk_score >= ?", merchantID, 70.0).
		Count(&highRiskOrdersFlagged).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to count high risk orders",
		})
	}

	executionMode := "ACTIVE"
	daysRemaining := 0
	if !merchant.IsActive || time.Now().Before(merchant.ShadowModeEndsAt) {
		executionMode = "SHADOW"
	}

	if time.Now().Before(merchant.ShadowModeEndsAt) {
		daysRemaining = int(time.Until(merchant.ShadowModeEndsAt).Hours() / 24)
		if daysRemaining < 0 {
			daysRemaining = 0
		}
	}

	estimatedLossPrevented := float64(highRiskOrdersFlagged) * 150.00

	shouldShowUpgradePrompt := false
	daysPastShadowMode := 0

	if !merchant.IsActive && time.Now().After(merchant.ShadowModeEndsAt) {
		daysPast := int(time.Since(merchant.ShadowModeEndsAt).Hours() / 24)
		daysPastShadowMode = daysPast
		daysRemaining = 0
		if daysPast%3 == 0 {
			shouldShowUpgradePrompt = true
		}
	} else {
		shouldShowUpgradePrompt = false
	}

	var threeDayHighRiskCount int64
	if err := h.pg.WithContext(c.UserContext()).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND predicted_risk_score >= ? AND created_at >= NOW() - INTERVAL '3 days'", merchantID, 70.0).
		Count(&threeDayHighRiskCount).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to count trailing high risk orders",
		})
	}
	threeDayTrailingLoss := float64(threeDayHighRiskCount) * 150.00

	daysActive := time.Since(merchant.CreatedAt).Hours() / 24
	if daysActive < 1.0 {
		daysActive = 1.0
	}
	lifetimeLoss := float64(highRiskOrdersFlagged) * 150.00
	dailyBurn := lifetimeLoss / daysActive
	projectedMonthlyLoss := dailyBurn * 30.0

	insights := domain.InsightsResponse{
		TotalOrdersAnalyzed:       int(totalOrdersAnalyzed),
		HighRiskOrdersFlagged:     int(highRiskOrdersFlagged),
		EstimatedLossPrevented:     estimatedLossPrevented,
		ExecutionMode:             executionMode,
		DaysRemainingInShadowMode: daysRemaining,
		ShouldShowUpgradePrompt:   shouldShowUpgradePrompt,
		DaysPastShadowMode:        daysPastShadowMode,
		ThreeDayTrailingLoss:      threeDayTrailingLoss,
		ProjectedMonthlyLoss:      projectedMonthlyLoss,
	}

	return c.JSON(insights)
}
