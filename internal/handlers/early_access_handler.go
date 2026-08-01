package handlers

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/notify"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EarlyAccessHandler handles all early-access request endpoints.
type EarlyAccessHandler struct {
	pg *gorm.DB
}

// NewEarlyAccessHandler constructs an EarlyAccessHandler.
func NewEarlyAccessHandler(pg *gorm.DB) *EarlyAccessHandler {
	return &EarlyAccessHandler{pg: pg}
}

// ==========================================
// POST /v1/early-access
// ==========================================

// SubmitRequest handles pre-login and post-login early-access sign-ups.
// No authentication required — works for both visitors and logged-in merchants.
//
// Body: { "email": "...", "feature_name": "..." }
//
// Returns: { "success": true, "total_count": N }
// Returns 409 if (email, feature_name) already exists (idempotent duplicate).
func (h *EarlyAccessHandler) SubmitRequest(c *fiber.Ctx) error {
	var req struct {
		Email       string `json:"email"`
		FeatureName string `json:"feature_name"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "invalid request body",
		})
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FeatureName = strings.TrimSpace(req.FeatureName)

	// Validate email (basic — not a full RFC 5322 parse)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "valid email is required",
		})
	}

	// Validate feature_name against the known set
	feature := domain.EarlyAccessFeature(req.FeatureName)
	if !domain.ValidEarlyAccessFeatures[feature] {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "feature_name must be one of: inventory_forecasting, demand_planning, logistics_optimisation, inventory_health, supplier_management, buyer_intelligence, network_intel, model_performance, growth_ads, crm_enrichment",
		})
	}

	// Resolve optional merchant ID from the API-key auth local set by middleware
	// (this endpoint is un-gated, so this may be empty)
	merchantID := ""
	if mid, ok := c.Locals("merchant_id").(string); ok {
		merchantID = mid
	}

	now := time.Now().UTC()
	record := domain.EarlyAccessRequest{
		MerchantID:  merchantID,
		Email:       req.Email,
		FeatureName: feature,
		Status:      domain.EarlyAccessPending,
		RequestedAt: now,
	}

	// Insert with ON CONFLICT DO NOTHING — treat a duplicate as success (idempotent)
	result := h.pg.WithContext(c.UserContext()).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "email"}, {Name: "feature_name"}},
			DoNothing: true,
		}).
		Create(&record)

	if result.Error != nil {
		slog.Error("early_access: failed to insert request",
			"email", req.Email, "feature", feature, "error", result.Error)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "failed to record early access request",
		})
	}

	// Count total signups for this feature (for the live counter)
	var totalCount int64
	h.pg.WithContext(c.UserContext()).
		Model(&domain.EarlyAccessRequest{}).
		Where("feature_name = ?", feature).
		Count(&totalCount)

	// If this was actually a new insert (not a duplicate), fire the alert in background
	if result.RowsAffected > 0 {
		go func(n notify.EarlyAccessNotification) {
			notify.SendEarlyAccessAlert(n)
		}(notify.EarlyAccessNotification{
			Email:       req.Email,
			FeatureName: string(feature),
			MerchantID:  merchantID,
			RequestedAt: now,
			TotalCount:  totalCount,
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success":     true,
		"total_count": totalCount,
	})
}

// ==========================================
// GET /v1/early-access/count?feature=...
// ==========================================

// GetCount returns the number of early-access requests for a given feature.
// Public — no auth required. Powers the live counter on the frontend.
//
// Query params: feature (required) — one of the valid EarlyAccessFeature values
//
// Returns: { "feature_name": "...", "count": N }
func (h *EarlyAccessHandler) GetCount(c *fiber.Ctx) error {
	featureParam := strings.TrimSpace(c.Query("feature"))
	if featureParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "feature query param is required",
		})
	}

	feature := domain.EarlyAccessFeature(featureParam)
	if !domain.ValidEarlyAccessFeatures[feature] {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid feature name",
		})
	}

	var count int64
	if err := h.pg.WithContext(c.UserContext()).
		Model(&domain.EarlyAccessRequest{}).
		Where("feature_name = ?", feature).
		Count(&count).Error; err != nil {
		slog.Error("early_access: count query failed", "feature", feature, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to retrieve count",
		})
	}

	return c.JSON(fiber.Map{
		"feature_name": string(feature),
		"count":        count,
	})
}

// ==========================================
// GET /v1/early-access/admin
// ==========================================

// AdminListRequests returns all early-access requests, paginated.
// Admin auth required (X-Admin-Key header, enforced by RequireAdminKey middleware).
//
// Query params:
//
//	page     (default 1)
//	per_page (default 25, max 100)
//	feature  (optional filter)
//	status   (optional filter: pending | approved | notified)
func (h *EarlyAccessHandler) AdminListRequests(c *fiber.Ctx) error {
	page, _ := strconv.Atoi(c.Query("page", "1"))
	perPage, _ := strconv.Atoi(c.Query("per_page", "25"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 25
	}
	offset := (page - 1) * perPage

	db := h.pg.WithContext(c.UserContext()).Model(&domain.EarlyAccessRequest{})

	// Optional filters
	if f := strings.TrimSpace(c.Query("feature")); f != "" {
		feature := domain.EarlyAccessFeature(f)
		if !domain.ValidEarlyAccessFeatures[feature] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid feature filter",
			})
		}
		db = db.Where("feature_name = ?", feature)
	}

	if s := strings.TrimSpace(c.Query("status")); s != "" {
		status := domain.EarlyAccessStatus(s)
		switch status {
		case domain.EarlyAccessPending, domain.EarlyAccessApproved, domain.EarlyAccessNotified:
			db = db.Where("status = ?", status)
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "status must be one of: pending, approved, notified",
			})
		}
	}

	// Count total (before pagination) for the response envelope
	var total int64
	if err := db.Count(&total).Error; err != nil {
		slog.Error("early_access admin: count failed", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to count requests",
		})
	}

	var requests []domain.EarlyAccessRequest
	if err := db.
		Order("requested_at DESC").
		Limit(perPage).
		Offset(offset).
		Find(&requests).Error; err != nil {
		// Ignore record-not-found; return empty list
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			slog.Error("early_access admin: list query failed", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to list requests",
			})
		}
	}

	return c.JSON(fiber.Map{
		"success":  true,
		"total":    total,
		"page":     page,
		"per_page": perPage,
		"data":     requests,
	})
}
