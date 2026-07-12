package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"log/slog"
	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// RequireAPIKey checks Upstash Redis first, then falls back to Postgres
func RequireAPIKey(pg *gorm.DB, redisClient *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Extract the key from the request header
		apiKey := c.Get("X-API-Key")

		// 2. If it's missing entirely, reject instantly
		if apiKey == "" {
			slog.Warn("auth blocked missing key", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Missing X-API-Key header. Are you a registered merchant?",
			})
		}
		
		// ⏱️ Start the 45ms countdown timer
		ctx, cancel := context.WithTimeout(c.UserContext(), 45*time.Millisecond)
		defer cancel() // CRITICAL: Destroy timer to prevent memory leaks
		
		hashedKey := crypto.HashAPIKey(apiKey)
		cacheKey := "auth:apikey:" + hashedKey

		// ==========================================
		// THE FAST PATH: Check Redis First
		// ==========================================
		cachedMerchant, err := redisClient.Get(ctx, cacheKey).Result()
		if err == nil {
			var merchant domain.Merchant
			if unmarshalErr := json.Unmarshal([]byte(cachedMerchant), &merchant); unmarshalErr == nil {
				
				// V2 SECURITY: Ensure the cached merchant hasn't been deactivated
				if !merchant.IsActive {
					slog.Warn("auth blocked inactive key in cache", "ip", c.IP())
					return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
						"success": false,
						"error":   "API Key deactivated. Access Denied.",
					})
				}

				revokeKey := "revoked:merchant:" + merchant.ID
				// M1 FIX: Use a fresh short-lived context for the revocation check,
				// not the 45ms ctx (which may already be near expiry) and not the
				// bare c.UserContext() (which has no deadline at all).
				revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer revokeCancel()
				revoked, _ := redisClient.Get(revokeCtx, revokeKey).Result()
				if revoked == "1" {
					slog.Warn("auth blocked: merchant revoked since cache was populated",
						"merchant_id", merchant.ID, "ip", c.IP())
					return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
						"success": false,
						"error":   "API Key deactivated. Access Denied.",
					})
				}

				// CACHE HIT! Set BOTH backpack variables to keep everything happy
				c.Locals("kotman.merchant", merchant)
				c.Locals("kotman.merchant_id", merchant.ID)
				return c.Next()
			}
		}

		// ==========================================
		// THE SLOW PATH: Query Postgres
		// ==========================================
		var merchant domain.Merchant
		
		// V2 SECURITY: We now explicitly check that is_active = true in the database
		err = pg.WithContext(ctx).Where("api_key_hash = ? AND is_active = ?", hashedKey, true).First(&merchant).Error
		
		if err != nil {
			slog.Warn("auth blocked invalid or inactive key", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Invalid API Key. Access Denied.",
			})
		}

		// Defense-in-depth: constant-time comparison prevents Go-level timing leak
		// even though the Postgres index lookup itself is not constant-time.
		if subtle.ConstantTimeCompare([]byte(hashedKey), []byte(merchant.APIKeyHash)) != 1 {
			slog.Warn("auth key mismatch after db lookup", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Invalid API Key. Access Denied.",
			})
		}

		// M2 FIX: Use a background context for the cache write so it is not
		// constrained by the 45ms request context, which may already be
		// exhausted after a slow Postgres lookup.
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cacheCancel()
		merchantJSON, _ := json.Marshal(merchant)
		// Save to Redis with a 5-Minute Time-To-Live (TTL)
		redisClient.Set(cacheCtx, cacheKey, merchantJSON, 5*time.Minute)

		slog.Info("auth granted",
			"store", merchant.StoreName,
			"merchant_id", merchant.ID,
			"ip", c.IP(),
		)

		// Set BOTH backpack variables
		c.Locals("kotman.merchant", merchant)
		c.Locals("kotman.merchant_id", merchant.ID)

		return c.Next()
	}
}

// RequireShopifyHMAC verifies incoming webhooks mathematically
// RequireShopifyHMAC now accepts the secret at startup to avoid runtime system calls
func RequireShopifyHMAC(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		shopifySignature := c.Get("X-Shopify-Hmac-Sha256")
		if shopifySignature == "" {
			slog.Warn("webhook blocked missing signature", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Missing Signature",
			})
		}

		// The secret is already safely in memory here! No os.Getenv needed.
		rawBody := c.Body()

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(rawBody)
		
		calculatedMAC := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		if !hmac.Equal([]byte(shopifySignature), []byte(calculatedMAC)) {
			slog.Warn("webhook cryptographic mismatch", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Cryptographic Mismatch",
			})
		}

		slog.Info("webhook signature verified securely", "ip", c.IP())
		return c.Next()
	}
}
// RequireWooCommerceHMAC verifies WooCommerce webhooks mathematically
func RequireWooCommerceHMAC(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		wooSignature := c.Get("X-Wc-Webhook-Signature")
		if wooSignature == "" {
			slog.Warn("woo webhook blocked missing signature", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Missing Signature",
			})
		}

		rawBody := c.Body()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(rawBody)
		
		calculatedMAC := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		if !hmac.Equal([]byte(wooSignature), []byte(calculatedMAC)) {
			slog.Warn("woo cryptographic mismatch", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Cryptographic Mismatch",
			})
		}

		slog.Info("woo webhook signature verified securely", "ip", c.IP())
		return c.Next()
	}
}

// RequireMagentoAuth verifies Magento webhooks (usually via Bearer Token)
func RequireMagentoAuth(secretToken string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		expectedHeader := "Bearer " + secretToken

		// FIX 1: Prevent timing attacks using ConstantTimeCompare
		if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedHeader)) != 1 {
			slog.Warn("magento webhook unauthorized", "ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Unauthorized",
			})
		}

		slog.Info("magento webhook verified securely", "ip", c.IP())
		return c.Next()
	}
}