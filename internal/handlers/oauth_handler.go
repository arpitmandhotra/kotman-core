package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/backfill"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/shopify"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/woocommerce"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type OAuthHandler struct {
	pg    *gorm.DB
	redis *redis.Client
}

func NewOAuthHandler(pg *gorm.DB, redisClient *redis.Client) *OAuthHandler {
	return &OAuthHandler{
		pg:    pg,
		redis: redisClient,
	}
}

// -----------------------------------------------------------------------------
// SHOPIFY OAUTH HANDLERS
// -----------------------------------------------------------------------------

var shopPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*\.myshopify\.com$`)

func (h *OAuthHandler) HandleShopifyInstall(c *fiber.Ctx) error {
	shop := c.Query("shop")
	if !shopPattern.MatchString(shop) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid shop parameter format",
		})
	}

	// Generate 32-byte cryptographically secure random state
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to generate secure state",
		})
	}
	state := hex.EncodeToString(stateBytes)

	// Save state in Redis with 10 minute TTL
	ctx := c.UserContext()
	stateKey := "oauth:state:" + state
	if err := h.redis.Set(ctx, stateKey, shop, 10*time.Minute).Err(); err != nil {
		slog.Error("failed to save oauth state in redis", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to initialize auth session",
		})
	}

	authURL := shopify.BuildAuthorizationURL(shop, state)
	return c.Redirect(authURL)
}

func (h *OAuthHandler) HandleShopifyCallback(c *fiber.Ctx) error {
	ctx := c.UserContext()
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")
	if clientSecret == "" {
		slog.Error("SHOPIFY_CLIENT_SECRET is missing in environment variables")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal auth configuration error",
		})
	}

	// Parse query params to url.Values
	queryParams := make(url.Values)
	c.Request().URI().QueryArgs().VisitAll(func(key, value []byte) {
		queryParams.Set(string(key), string(value))
	})

	// FIRST: VerifyCallbackHMAC
	if !shopify.VerifyCallbackHMAC(queryParams, clientSecret) {
		slog.Warn("shopify callback rejected: invalid HMAC signature")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "invalid callback signature",
		})
	}

	// SECOND: verify state from Redis
	state := c.Query("state")
	shop := c.Query("shop")
	stateKey := "oauth:state:" + state

	cachedShop, err := h.redis.Get(ctx, stateKey).Result()
	if err != nil {
		slog.Warn("shopify oauth state lookup failed or expired", "state", state)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "invalid or expired auth session",
		})
	}
	h.redis.Del(ctx, stateKey) // single use deletion

	if cachedShop != shop {
		slog.Warn("shopify oauth state shop mismatch", "cached", cachedShop, "received", shop)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "security session mismatch",
		})
	}

	code := c.Query("code")
	if code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing auth code",
		})
	}

	// THIRD: ExchangeCodeForToken
	tokenResp, err := shopify.ExchangeCodeForToken(ctx, shop, code)
	if err != nil {
		slog.Error("failed to exchange shopify code for token", "shop", shop, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to exchange access token",
		})
	}

	// FOURTH: Generate new API key
	apiKey, err := GenerateAPIKey()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to generate API credentials",
		})
	}

	// FIFTH: Postgres Transaction to save Merchant and PlatformCredential
	var merchant domain.Merchant
	err = h.pg.Transaction(func(tx *gorm.DB) error {
		// Look up existing PlatformCredential or create one
		var cred domain.PlatformCredential
		err := tx.Where("platform = ? AND shop_domain = ?", "shopify", shop).First(&cred).Error
		if err == nil {
			// Find existing merchant
			if err := tx.Where("id = ?", cred.MerchantID).First(&merchant).Error; err != nil {
				return err
			}
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			// Create new Merchant
			merchant = domain.Merchant{
				ID:        uuid.New().String(),
				StoreName: shop,
				APIKey:    apiKey,
				Platform:  "shopify",
				IsActive:  true,
			}
			if err := tx.Create(&merchant).Error; err != nil {
				return err
			}

			// Initialize default merchant settings
			settings := domain.MerchantSettings{
				MerchantID: merchant.ID,
			}
			if err := tx.Create(&settings).Error; err != nil {
				return err
			}
		} else {
			return err
		}

		// Encrypt tokens
		encAccess, err := crypto.EncryptToken(tokenResp.AccessToken)
		if err != nil {
			return err
		}
		var encRefresh string
		if tokenResp.RefreshToken != "" {
			encRefresh, err = crypto.EncryptToken(tokenResp.RefreshToken)
			if err != nil {
				return err
			}
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		now := time.Now()

		// Upsert PlatformCredential
		cred.MerchantID = merchant.ID
		cred.Platform = "shopify"
		cred.ShopDomain = shop
		cred.AccessTokenEncrypted = encAccess
		cred.RefreshTokenEncrypted = encRefresh
		cred.Scopes = tokenResp.Scope
		cred.TokenExpiresAt = &expiresAt
		cred.LastRefreshedAt = &now
		cred.InstalledAt = now
		cred.IsActive = true
		cred.UninstalledAt = nil

		if err := tx.Save(&cred).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		slog.Error("failed database transaction inside oauth callback", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "database transaction failed",
		})
	}

	// SIXTH: Kick off async history backfill
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if backfillErr := backfill.BackfillOrderHistory(backfillCtx, merchant.ID, "shopify"); backfillErr != nil {
			slog.Error("shopify historical order backfill failed", "merchant_id", merchant.ID, "error", backfillErr)
		}
	}()

	// Redirect to dashboard welcome screen with API key in URL fragment
	welcomeURL := os.Getenv("DASHBOARD_WELCOME_URL")
	if welcomeURL == "" {
		welcomeURL = "http://localhost:3000/welcome"
	}
	return c.Redirect(fmt.Sprintf("%s#api_key=%s", welcomeURL, merchant.APIKey))
}

// -----------------------------------------------------------------------------
// WOOCOMMERCE HANDLERS
// -----------------------------------------------------------------------------

func (h *OAuthHandler) HandleWooCommerceAuthStart(c *fiber.Ctx) error {
	storeURL := c.Query("store_url")
	if storeURL == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing store_url query parameter",
		})
	}

	// Generate tracking ID and merchant UUID
	trackingID := uuid.New().String()
	merchantID := uuid.New().String()

	// Store pending state in Redis for 15 minutes
	ctx := c.UserContext()
	pendingKey := "woo:pending:" + trackingID
	pendingVal, _ := json.Marshal(map[string]string{
		"store_url":   storeURL,
		"merchant_id": merchantID,
	})
	if err := h.redis.Set(ctx, pendingKey, string(pendingVal), 15*time.Minute).Err(); err != nil {
		slog.Error("failed saving woo pending session in redis", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to initialize oauth session",
		})
	}

	// Construct callback & return URLs
	apiDomain := os.Getenv("API_DOMAIN")
	if apiDomain == "" {
		apiDomain = "http://localhost:3000"
	}
	callbackURL := fmt.Sprintf("%s/auth/woocommerce/callback?tracking_id=%s", apiDomain, trackingID)
	returnURL := fmt.Sprintf("%s/auth/woocommerce/return?tracking_id=%s", apiDomain, trackingID)

	appName := os.Getenv("WOOCOMMERCE_APP_NAME")
	if appName == "" {
		appName = "Kotman RTO"
	}

	authURL, err := woocommerce.BuildAuthorizeURL(storeURL, appName, returnURL, callbackURL, merchantID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": err.Error(),
		})
	}

	return c.Redirect(authURL)
}

type WooCommerceCallbackPayload struct {
	KeyID          int    `json:"key_id"`
	UserID         string `json:"user_id"`
	ConsumerKey    string `json:"consumer_key"`
	ConsumerSecret string `json:"consumer_secret"`
	Permissions    string `json:"key_permissions"`
}

func (h *OAuthHandler) HandleWooCommerceCallback(c *fiber.Ctx) error {
	ctx := c.UserContext()
	trackingID := c.Query("tracking_id")
	if trackingID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing tracking_id query parameter",
		})
	}

	// Retrieve pending state from Redis
	pendingKey := "woo:pending:" + trackingID
	pendingData, err := h.redis.Get(ctx, pendingKey).Result()
	if err != nil {
		slog.Warn("woocommerce pending session expired or missing", "tracking_id", trackingID)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid or expired pending session",
		})
	}

	var pending map[string]string
	if err := json.Unmarshal([]byte(pendingData), &pending); err != nil {
		slog.Error("failed unmarshalling woo pending state", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal state parsing error",
		})
	}

	expectedStoreURL := pending["store_url"]
	merchantID := pending["merchant_id"]

	var payload WooCommerceCallbackPayload
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid JSON body",
		})
	}

	// Verify merchantID matches
	if payload.UserID != merchantID {
		slog.Warn("woocommerce callback user_id mismatch", "expected", merchantID, "received", payload.UserID)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "unauthorized callback data source",
		})
	}

	// Encrypt keys
	encKey, err := crypto.EncryptToken(payload.ConsumerKey)
	if err != nil {
		slog.Error("failed encrypting consumer key", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed encrypting credentials",
		})
	}
	encSecret, err := crypto.EncryptToken(payload.ConsumerSecret)
	if err != nil {
		slog.Error("failed encrypting consumer secret", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed encrypting credentials",
		})
	}

	apiKey, err := GenerateAPIKey()
	if err != nil {
		slog.Error("failed generating API key", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed generating credentials",
		})
	}

	// Transaction to insert Merchant + PlatformCredential
	err = h.pg.Transaction(func(tx *gorm.DB) error {
		merchant := domain.Merchant{
			ID:        merchantID,
			StoreName: expectedStoreURL,
			APIKey:    apiKey,
			Platform:  "woocommerce",
			IsActive:  true,
		}
		if err := tx.Create(&merchant).Error; err != nil {
			return err
		}

		settings := domain.MerchantSettings{
			MerchantID: merchantID,
		}
		if err := tx.Create(&settings).Error; err != nil {
			return err
		}

		cred := domain.PlatformCredential{
			MerchantID:              merchantID,
			Platform:                "woocommerce",
			ShopDomain:              expectedStoreURL,
			ConsumerKeyEncrypted:    encKey,
			ConsumerSecretEncrypted: encSecret,
			Scopes:                  payload.Permissions,
			InstalledAt:             time.Now(),
			IsActive:                true,
		}
		if err := tx.Create(&cred).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		slog.Error("failed saving woocommerce credentials in transaction", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "database persistence failed",
		})
	}

	// Save generated key to Redis for the redirect return page to pick up
	keyStoreKey := "woo:key:" + trackingID
	h.redis.Set(ctx, keyStoreKey, apiKey, 15*time.Minute)

	// Clean pending from Redis
	h.redis.Del(ctx, pendingKey)

	// Kick off historical backfill async
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if backfillErr := backfill.BackfillOrderHistory(backfillCtx, merchantID, "woocommerce"); backfillErr != nil {
			slog.Error("woocommerce historical order backfill failed", "merchant_id", merchantID, "error", backfillErr)
		}
	}()

	return c.SendStatus(fiber.StatusOK)
}

func (h *OAuthHandler) HandleWooCommerceReturn(c *fiber.Ctx) error {
	ctx := c.UserContext()
	trackingID := c.Query("tracking_id")
	if trackingID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing tracking_id",
		})
	}

	keyStoreKey := "woo:key:" + trackingID
	apiKey, err := h.redis.Get(ctx, keyStoreKey).Result()
	if err != nil {
		slog.Warn("woocommerce redirect return session expired or missing", "tracking_id", trackingID)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "authorization credentials not found or expired",
		})
	}
	h.redis.Del(ctx, keyStoreKey)

	welcomeURL := os.Getenv("DASHBOARD_WELCOME_URL")
	if welcomeURL == "" {
		welcomeURL = "http://localhost:3000/welcome"
	}
	return c.Redirect(fmt.Sprintf("%s#api_key=%s", welcomeURL, apiKey))
}
