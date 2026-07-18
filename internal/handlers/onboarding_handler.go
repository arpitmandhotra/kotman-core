package handlers

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OnboardingHandler struct {
	pg    *gorm.DB
	redis *redis.Client
}

func NewOnboardingHandler(pgDB *gorm.DB, redisClient *redis.Client) *OnboardingHandler {
	return &OnboardingHandler{
		pg:    pgDB,
		redis: redisClient,
	}
}

type RegisterRequest struct {
	StoreName string `json:"store_name"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	Platform  string `json:"platform"` // "shopify" | "woocommerce" | "magento"
}

// RegisterMerchant handles merchant zero-touch registration
func (h *OnboardingHandler) RegisterMerchant(c *fiber.Ctx) error {
	var req RegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "Invalid request body",
		})
	}

	req.StoreName = strings.TrimSpace(req.StoreName)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Password = req.Password

	if req.StoreName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "store_name is required",
		})
	}

	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "valid email is required",
		})
	}

	if len(req.Password) < 8 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "password must be at least 8 characters long",
		})
	}

	// Check if email already registered
	var existingCount int64
	if err := h.pg.Model(&domain.Merchant{}).Where("email = ?", req.Email).Count(&existingCount).Error; err == nil && existingCount > 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "email already registered",
		})
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("failed to bcrypt hash password", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to secure password",
		})
	}

	merchantID := uuid.New().String()
	merchant := domain.Merchant{
		ID:            merchantID,
		StoreName:     req.StoreName,
		APIKeyHash:    "", // no API key revealed until email verified
		Email:         req.Email,
		PasswordHash:  string(hashedPassword),
		EmailVerified: false,
		Platform:      req.Platform,
		IsActive:      true, // active from day one
		Tier:          domain.TierFree,
	}

	err = h.pg.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&merchant).Error; err != nil {
			return err
		}

		settings := domain.MerchantSettings{
			MerchantID: merchantID,
		}
		return tx.Create(&settings).Error
	})

	if err != nil {
		slog.Error("failed to create merchant record during registration", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to register merchant",
		})
	}

	// Generate OTP and send verification email
	otp := service.GenerateOTP()
	if err := service.StoreOTP(c.UserContext(), h.redis, req.Email, otp); err != nil {
		slog.Error("failed to store verification OTP in Redis", "error", err)
	}

	_ = service.SendVerificationEmail(req.Email, otp)

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "verification email sent. please verify your email to activate API access.",
	})
}

// VerifyEmail verifies the 6-digit OTP and reveals the generated API Key
func (h *OnboardingHandler) VerifyEmail(c *fiber.Ctx) error {
	ctx := c.UserContext()
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid request body",
		})
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Code = strings.TrimSpace(req.Code)

	if req.Email == "" || req.Code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "email and code are required",
		})
	}

	var merchant domain.Merchant
	if err := h.pg.Where("email = ?", req.Email).First(&merchant).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "merchant not found",
		})
	}

	ok, err := service.VerifyOTP(ctx, h.redis, req.Email, req.Code)
	if err != nil {
		slog.Error("failed to verify OTP", "email", req.Email, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "verification system error",
		})
	}

	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Incorrect or expired code",
		})
	}

	apiKey, err := GenerateAPIKey()
	if err != nil {
		slog.Error("failed generating API key", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed generating API key",
		})
	}

	merchant.EmailVerified = true
	merchant.APIKeyHash = crypto.HashAPIKey(apiKey)

	if err := h.pg.Save(&merchant).Error; err != nil {
		slog.Error("failed to save verified merchant", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "database persistence failed",
		})
	}

	return c.JSON(fiber.Map{
		"api_key":    apiKey,
		"store_name": merchant.StoreName,
		"warning":    "",
	})
}

// ResendVerification handles resending the email OTP code
func (h *OnboardingHandler) ResendVerification(c *fiber.Ctx) error {
	ctx := c.UserContext()
	var req struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid request body",
		})
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "email is required",
		})
	}

	// Rate limiting: max 3 resends per email per hour
	limitKey := "resend_limit:" + req.Email
	count, err := h.redis.Get(ctx, limitKey).Int()
	if err == nil && count >= 3 {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error": "too many verification codes sent. please wait an hour",
		})
	}

	// Increment code limit counter
	newCount, err := h.redis.Incr(ctx, limitKey).Result()
	if err == nil {
		if newCount == 1 {
			h.redis.Expire(ctx, limitKey, time.Hour)
		}
		if newCount > 3 {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "too many verification codes sent. please wait an hour",
			})
		}
	}

	var merchant domain.Merchant
	if err := h.pg.Where("email = ?", req.Email).First(&merchant).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Do not leak whether the email exists
			return c.JSON(fiber.Map{
				"success": true,
				"message": "If this email is registered, a code will be sent",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "database error",
		})
	}

	if merchant.EmailVerified {
		return c.JSON(fiber.Map{
			"success": true,
			"message": "Email already verified",
		})
	}

	otp := service.GenerateOTP()
	if err := service.StoreOTP(ctx, h.redis, req.Email, otp); err != nil {
		slog.Error("failed storing OTP in resend", "error", err)
	}

	_ = service.SendVerificationEmail(req.Email, otp)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Verification code sent",
	})
}

// Login handles merchant authentication and regenerates the API Key on success
func (h *OnboardingHandler) Login(c *fiber.Ctx) error {
	ctx := c.UserContext()
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid request body",
		})
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" || req.Password == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Incorrect email or password",
		})
	}

	var merchant domain.Merchant
	if err := h.pg.Where("LOWER(email) = LOWER(?)", req.Email).First(&merchant).Error; err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Incorrect email or password",
		})
	}

	if !merchant.EmailVerified {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Please verify your email before signing in",
		})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(merchant.PasswordHash), []byte(req.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Incorrect email or password",
		})
	}

	// Regenerate the API Key
	apiKey, err := GenerateAPIKey()
	if err != nil {
		slog.Error("failed generating API key during login", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal credential rotation failure",
		})
	}

	oldHash := merchant.APIKeyHash
	merchant.APIKeyHash = crypto.HashAPIKey(apiKey)

	if err := h.pg.Save(&merchant).Error; err != nil {
		slog.Error("failed saving merchant rotated API Key", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "database persistence failed",
		})
	}

	// Invalidate older Redis auth cache key
	if oldHash != "" {
		h.redis.Del(ctx, "auth:apikey:"+oldHash)
	}

	return c.JSON(fiber.Map{
		"api_key":    apiKey,
		"store_name": merchant.StoreName,
	})
}

type UpdateSettingsRequest struct {
	MetaPixelID        *string `json:"meta_pixel_id"`
	MetaAccessToken    *string `json:"meta_access_token"`
	MetaAdAccountID    *string `json:"meta_ad_account_id"`
	MetaTestEventCode  *string `json:"meta_test_event_code"`
	MetaCAPIEnabled    *bool   `json:"meta_capi_enabled"`
	CapiDatasetID      *string `json:"capi_dataset_id"`
}

// UpdateSettings updates the merchant configuration settings (CAPI, wallet, etc.)
// Route: PATCH /v1/merchants/settings
func (h *OnboardingHandler) UpdateSettings(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	var req UpdateSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": "invalid body"})
	}

	ctx := c.UserContext()
	var merchant domain.Merchant
	if err := h.pg.WithContext(ctx).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// Field validation
	errors := fiber.Map{}
	if req.MetaPixelID != nil && *req.MetaPixelID != "" {
		isNumeric := true
		for _, r := range *req.MetaPixelID {
			if r < '0' || r > '9' {
				isNumeric = false
				break
			}
		}
		if !isNumeric {
			errors["meta_pixel_id"] = "Meta Pixel ID must be numeric"
		}
	}

	if req.MetaAccessToken != nil && *req.MetaAccessToken != "" {
		if !strings.HasPrefix(*req.MetaAccessToken, "EAAG") {
			errors["meta_access_token"] = "Meta Access Token must start with 'EAAG'"
		}
	}

	if req.MetaAdAccountID != nil && *req.MetaAdAccountID != "" {
		if !strings.HasPrefix(*req.MetaAdAccountID, "act_") {
			errors["meta_ad_account_id"] = "Meta Ad Account ID must start with 'act_'"
		}
	}

	if len(errors) > 0 {
		return c.Status(400).JSON(fiber.Map{
			"success": false,
			"errors":  errors,
		})
	}

	// Load or create MerchantSettings
	var settings domain.MerchantSettings
	err := h.pg.WithContext(ctx).Where("merchant_id = ?", merchantID).First(&settings).Error
	if err != nil {
		if gorm.ErrRecordNotFound == err {
			settings = domain.MerchantSettings{MerchantID: merchantID}
			if createErr := h.pg.WithContext(ctx).Create(&settings).Error; createErr != nil {
				return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to initialize settings"})
			}
		} else {
			return c.Status(500).JSON(fiber.Map{"success": false, "error": "database error"})
		}
	}

	// Gate MetaCAPIEnabled on Growth + Ads plan
	if req.MetaCAPIEnabled != nil && *req.MetaCAPIEnabled {
		if merchant.Tier != domain.TierGrowthAds {
			return c.Status(403).JSON(fiber.Map{
				"error":         "Meta CAPI enrichment requires the Growth + Ads plan",
				"tier_required": "growth_ads",
			})
		}
	}

	// Apply updates
	updates := map[string]interface{}{}
	if req.MetaPixelID != nil {
		updates["meta_pixel_id"] = *req.MetaPixelID
	}
	if req.MetaAccessToken != nil {
		if *req.MetaAccessToken != "" {
			encToken, err := crypto.EncryptToken(*req.MetaAccessToken)
			if err != nil {
				slog.Error("failed to encrypt Meta token", "error", err)
				return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed securing access token"})
			}
			updates["meta_access_token_encrypted"] = encToken
		} else {
			updates["meta_access_token_encrypted"] = ""
		}
	}
	if req.MetaAdAccountID != nil {
		updates["meta_ad_account_id"] = *req.MetaAdAccountID
	}
	if req.MetaTestEventCode != nil {
		updates["meta_test_event_code"] = *req.MetaTestEventCode
	}
	if req.MetaCAPIEnabled != nil {
		updates["meta_capi_enabled"] = *req.MetaCAPIEnabled
	}
	if req.CapiDatasetID != nil {
		updates["capi_dataset_id"] = *req.CapiDatasetID
	}

	if len(updates) > 0 {
		if err := h.pg.WithContext(ctx).Model(&settings).Updates(updates).Error; err != nil {
			slog.Error("failed to update merchant settings", "merchant_id", merchantID, "error", err)
			return c.Status(500).JSON(fiber.Map{"success": false, "error": "failed to save settings"})
		}
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Settings updated successfully",
		"settings": settings,
	})
}

// JoinWaitlist handles POST /v1/waitlist/join
func (h *OnboardingHandler) JoinWaitlist(c *fiber.Ctx) error {
	var req struct {
		Email        string `json:"email"`
		StoreName    string `json:"store_name"`
		TierInterest string `json:"tier_interest"`
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "invalid JSON payload",
		})
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "valid email is required",
		})
	}

	tierInterest := strings.ToLower(strings.TrimSpace(req.TierInterest))
	if tierInterest == "" {
		tierInterest = "growth"
	}
	validInterests := map[string]bool{
		"growth":     true,
		"growth_ads": true,
		"rto_engine": true,
		"all":        true,
	}
	if !validInterests[tierInterest] {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"error":   "tier_interest must be one of: growth, growth_ads, rto_engine, all",
		})
	}

	// Optional authentication: check if merchant is logged in via X-API-Key
	merchantID := ""
	apiKey := c.Get("X-API-Key")
	if apiKey != "" {
		hashedKey := crypto.HashAPIKey(apiKey)
		var m domain.Merchant
		if err := h.pg.WithContext(c.UserContext()).Where("api_key_hash = ? AND is_active = ?", hashedKey, true).First(&m).Error; err == nil {
			merchantID = m.ID
		}
	}

	source := "dashboard"
	if merchantID == "" {
		source = "pricing_page"
	}

	entry := domain.WaitlistEntry{
		ID:           uuid.New().String(),
		Email:        email,
		StoreName:    req.StoreName,
		MerchantID:   merchantID,
		TierInterest: tierInterest,
		Source:       source,
	}

	err := h.pg.WithContext(c.UserContext()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		DoUpdates: clause.AssignmentColumns([]string{"tier_interest", "store_name", "merchant_id", "source", "updated_at"}),
	}).Create(&entry).Error

	if err != nil {
		slog.Error("failed to save waitlist entry", "email", email, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "failed to save waitlist entry",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "You're on the list. We'll reach out when Growth launches.",
	})
}

// GetWaitlist handles GET /v1/admin/waitlist
func (h *OnboardingHandler) GetWaitlist(c *fiber.Ctx) error {
	var entries []domain.WaitlistEntry
	var total int64

	query := h.pg.Model(&domain.WaitlistEntry{})
	if tier := c.Query("tier"); tier != "" {
		query = query.Where("tier_interest = ?", tier)
	}

	if err := query.Count(&total).Error; err != nil {
		slog.Error("failed to count waitlist entries", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "failed to query waitlist",
		})
	}

	limit := 100
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	offset := 0
	if o := c.Query("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	if err := query.Limit(limit).Offset(offset).Order("created_at DESC").Find(&entries).Error; err != nil {
		slog.Error("failed to fetch waitlist entries", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "failed to fetch waitlist",
		})
	}

	return c.JSON(fiber.Map{
		"entries": entries,
		"total":   total,
	})
}
