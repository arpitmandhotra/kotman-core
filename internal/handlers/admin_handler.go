package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/service"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type AdminHandler struct {
	pg     *gorm.DB
	csvSvc *service.CSVImportService
}

func NewAdminHandler(pg *gorm.DB, csvSvc *service.CSVImportService) *AdminHandler {
	return &AdminHandler{pg: pg, csvSvc: csvSvc}
}

// ValidateCSV handles POST /v1/admin/import-csv/validate
func (h *AdminHandler) ValidateCSV(c *fiber.Ctx) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing 'file' in form-data",
		})
	}

	platform := c.Query("platform", "generic")
	if formPlatform := c.FormValue("platform"); formPlatform != "" {
		platform = formPlatform
	}

	file, err := fileHeader.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to open file stream",
		})
	}
	defer file.Close()

	report, err := h.csvSvc.ValidateAndStage(c.Context(), file, platform)
	if err != nil {
		slog.Error("CSV validation failed", "error", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(report)
}

// CommitCSVRequest matches the expected body for /commit
type CommitCSVRequest struct {
	PreviewToken string `json:"preview_token"`
	Platform     string `json:"platform"`
}

// CommitCSV handles POST /v1/admin/import-csv/commit
func (h *AdminHandler) CommitCSV(c *fiber.Ctx) error {
	var req CommitCSVRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid JSON body",
		})
	}

	if req.PreviewToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing 'preview_token'",
		})
	}

	platform := req.Platform
	if platform == "" {
		platform = c.Query("platform", "generic")
	}

	merchantID, ok := c.Locals("kotman.merchant_id").(string)
	if !ok || merchantID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "missing or invalid merchant context",
		})
	}

	result, err := h.csvSvc.Commit(c.Context(), req.PreviewToken, merchantID, platform)
	if err != nil {
		slog.Error("CSV commit failed", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(result)
}

// GetRecentBlocks fetches the latest scammers caught by the Kotman engine
func (h *AdminHandler) GetRecentBlocks(c *fiber.Ctx) error {
	merchantName := "Admin"
	var scammers []domain.TrustProfile

	// Reach into Cold Storage and grab the 50 most recently caught scammers
	err := h.pg.Order("locked_at desc").Limit(50).Find(&scammers).Error
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to retrieve the vault data",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success":      true,
		"merchant":     merchantName,
		"total_blocks": len(scammers),
		"data":         scammers,
	})
}

// OnboardMerchantRequest holds incoming data for creating a new Shopify client
type OnboardMerchantRequest struct {
	StoreName string `json:"store_name"`
}

// GenerateAPIKey generates a cryptographically secure 32-byte API key with prefix kt_live_
func GenerateAPIKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "kt_live_" + hex.EncodeToString(bytes), nil
}

// OnboardMerchant generates a secure API credential and inserts a new merchant profile using UUIDs
func (h *AdminHandler) OnboardMerchant(c *fiber.Ctx) error {
	var req OnboardMerchantRequest
	if err := c.BodyParser(&req); err != nil || req.StoreName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "A valid store_name string is required",
		})
	}

	// 1. Generate 32 bytes of cryptographically secure randomness
	apiKey, err := GenerateAPIKey()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to safely generate crypto random bytes",
		})
	}

	// 3. Assemble the updated Merchant schema
	merchant := domain.Merchant{
		StoreName:  req.StoreName,
		APIKeyHash: crypto.HashAPIKey(apiKey),
		IsActive:   true,
	}

	// 4. Persistence execution
	if err := h.pg.Create(&merchant).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to commit new merchant credentials to database",
		})
	}

	// 5. Return payload so you can hand this key over to your friend
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":     "Merchant registered successfully",
		"merchant_id": merchant.ID,
		"store_name":  merchant.StoreName,
		"api_key":     apiKey, // Store this — it cannot be recovered after this response
	})
}

func parseBillingMonth(monthStr string) (int, time.Month, error) {
	var year int
	var month int
	_, err := fmt.Sscanf(monthStr, "%d-%d", &year, &month)
	if err != nil {
		return 0, 0, err
	}
	return year, time.Month(month), nil
}

// GetBillingEvents handles GET /v1/admin/billing/events?merchant_id=&month=&requires_review=
func (h *AdminHandler) GetBillingEvents(c *fiber.Ctx) error {
	page, _ := strconv.Atoi(c.Query("page", "1"))
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	offset := (page - 1) * limit

	query := h.pg.Model(&domain.BillableEvent{})

	if merchantID := c.Query("merchant_id"); merchantID != "" {
		query = query.Where("merchant_id = ?", merchantID)
	}

	if month := c.Query("month"); month != "" {
		year, m, err := parseBillingMonth(month)
		if err == nil {
			start := time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0).Add(-time.Second)
			query = query.Where("created_at >= ? AND created_at <= ?", start, end)
		}
	}

	if reqReview := c.Query("requires_review"); reqReview != "" {
		val, err := strconv.ParseBool(reqReview)
		if err == nil {
			query = query.Where("requires_review = ?", val)
		}
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	var events []domain.BillableEvent
	if err := query.Order("created_at desc").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"total":  total,
		"page":   page,
		"limit":  limit,
		"events": events,
	})
}

// GetBillingSummary handles GET /v1/admin/billing/summary?merchant_id=&month=
func (h *AdminHandler) GetBillingSummary(c *fiber.Ctx) error {
	merchantID := c.Query("merchant_id")
	month := c.Query("month")

	if merchantID == "" || month == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "merchant_id and month are required query parameters",
		})
	}

	var accumulator domain.MerchantBillingAccumulator
	err := h.pg.Where("merchant_id = ? AND billing_month = ?", merchantID, month).First(&accumulator).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			accumulator = domain.MerchantBillingAccumulator{
				MerchantID:   merchantID,
				BillingMonth: month,
			}
		} else {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
	}

	year, m, err := parseBillingMonth(month)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid month format, use YYYY-MM"})
	}
	start := time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0).Add(-time.Second)

	var nativeCODCount int64
	var nativePrepaidCount int64
	var thirdPartyCODCount int64
	var thirdPartyPrepaidCount int64

	baseQuery := h.pg.Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND created_at >= ? AND created_at <= ?", merchantID, start, end)

	baseQuery.Session(&gorm.Session{}).Where("checkout_mode = ? AND payment_method = ?", "native", "cod").Count(&nativeCODCount)
	baseQuery.Session(&gorm.Session{}).Where("checkout_mode = ? AND payment_method = ?", "native", "prepaid").Count(&nativePrepaidCount)
	baseQuery.Session(&gorm.Session{}).Where("checkout_mode = ? AND payment_method = ?", "third_party", "cod").Count(&thirdPartyCODCount)
	baseQuery.Session(&gorm.Session{}).Where("checkout_mode = ? AND payment_method = ?", "third_party", "prepaid").Count(&thirdPartyPrepaidCount)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"merchant_id":                merchantID,
		"billing_month":              month,
		"total_events":               accumulator.TotalEvents,
		"total_fee_paise":            accumulator.TotalFeePaise,
		"is_invoiced":                accumulator.IsInvoiced,
		"native_cod_events":          nativeCODCount,
		"native_prepaid_events":      nativePrepaidCount,
		"third_party_cod_events":     thirdPartyCODCount,
		"third_party_prepaid_events": thirdPartyPrepaidCount,
	})
}

// GetInvoices handles GET /v1/admin/billing/invoices?status=pending
func (h *AdminHandler) GetInvoices(c *fiber.Ctx) error {
	status := c.Query("status")
	query := h.pg.Model(&domain.MerchantInvoice{})
	if status != "" {
		query = query.Where("status = ?", status)
	}

	var invoices []domain.MerchantInvoice
	if err := query.Order("created_at desc").Find(&invoices).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusOK).JSON(invoices)
}

type OverrideFeeRequest struct {
	MerchantID string `json:"merchant_id"`
	FeePaise   int    `json:"fee_paise"`
	Reason     string `json:"reason"`
}

// OverrideEventFee handles POST /v1/admin/billing/events/:event_id/override
func (h *AdminHandler) OverrideEventFee(c *fiber.Ctx) error {
	eventIDStr := c.Params("event_id")
	eventID, err := strconv.ParseUint(eventIDStr, 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid event_id"})
	}

	var req OverrideFeeRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body"})
	}

	req.MerchantID = strings.TrimSpace(req.MerchantID)
	if req.MerchantID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchant_id is required"})
	}

	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "override reason is required"})
	}
	if req.FeePaise < 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "fee_paise cannot be negative"})
	}

	err = h.pg.Transaction(func(tx *gorm.DB) error {
		var event domain.BillableEvent
		if err := tx.Where("id = ? AND merchant_id = ?", eventID, req.MerchantID).First(&event).Error; err != nil {
			return err
		}

		diff := req.FeePaise - event.FeePaise
		billingMonth := event.CreatedAt.Format("2006-01")

		var eventUpdate map[string]interface{}
		var accUpdate map[string]interface{}

		if !event.IsBillable && req.FeePaise > 0 {
			eventUpdate = map[string]interface{}{
				"fee_paise":   req.FeePaise,
				"is_billable": true,
			}
			accUpdate = map[string]interface{}{
				"total_events":    gorm.Expr("total_events + 1"),
				"total_fee_paise": gorm.Expr("total_fee_paise + ?", req.FeePaise),
			}
		} else if event.IsBillable && req.FeePaise == 0 {
			eventUpdate = map[string]interface{}{
				"fee_paise":   0,
				"is_billable": false,
			}
			accUpdate = map[string]interface{}{
				"total_events":    gorm.Expr("total_events - 1"),
				"total_fee_paise": gorm.Expr("total_fee_paise - ?", event.FeePaise),
			}
		} else {
			eventUpdate = map[string]interface{}{
				"fee_paise": req.FeePaise,
			}
			accUpdate = map[string]interface{}{
				"total_fee_paise": gorm.Expr("total_fee_paise + ?", diff),
			}
		}

		if err := tx.Model(&event).Updates(eventUpdate).Error; err != nil {
			return err
		}

		var accumulator domain.MerchantBillingAccumulator
		err := tx.Where("merchant_id = ? AND billing_month = ?", event.MerchantID, billingMonth).
			FirstOrCreate(&accumulator, domain.MerchantBillingAccumulator{
				MerchantID:   event.MerchantID,
				BillingMonth: billingMonth,
			}).Error
		if err != nil {
			return err
		}

		res := tx.Model(&accumulator).
			Where("merchant_id = ? AND billing_month = ?", event.MerchantID, billingMonth).
			Updates(accUpdate)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("accumulator update affected zero rows — potential race condition")
		}

		slog.Info("admin fee override executed successfully",
			"event_id", event.ID,
			"merchant_id", event.MerchantID,
			"old_fee", event.FeePaise,
			"new_fee", req.FeePaise,
			"reason", req.Reason,
		)

		return nil
	})

	if err != nil {
		slog.Error("failed executing admin fee override", "event_id", eventID, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Fee overridden successfully",
	})
}

// GetSubscriptionStatus returns all active and recently expired subscriptions.
// Route: GET /v1/admin/subscriptions
// Auth: RequireAdminKey middleware (existing)
func (h *AdminHandler) GetSubscriptionStatus(c *fiber.Ctx) error {
	var subs []domain.MerchantSubscription
	if err := h.pg.WithContext(c.UserContext()).
		Order("current_period_end desc").
		Limit(200).
		Find(&subs).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": "query failed"})
	}

	// Enrich with merchant store name for readability
	type enrichedSub struct {
		domain.MerchantSubscription
		StoreName string `json:"store_name"`
	}

	enriched := make([]enrichedSub, 0, len(subs))
	for _, s := range subs {
		var m domain.Merchant
		h.pg.WithContext(c.UserContext()).Select("store_name").Where("id = ?", s.MerchantID).First(&m)
		enriched = append(enriched, enrichedSub{
			MerchantSubscription: s,
			StoreName:            m.StoreName,
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"count":   len(enriched),
		"data":    enriched,
	})
}

// TODO (V1.1): Add POST /v1/admin/subscriptions/:id/cancel endpoint.
// For now, manually UPDATE merchant_subscriptions SET status='cancelled',
// cancelled_at=NOW() WHERE id=? and then UPDATE merchants SET
// has_cross_network_intel=false / has_crm_upsell_engine=false WHERE id=?