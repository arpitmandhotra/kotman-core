package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"encoding/json"
	"time"
	"github.com/redis/go-redis/v9"

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
			log.Printf("🚨 [AUTH] Blocked request missing API Key from IP: %s", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Missing X-API-Key header. Are you a registered merchant?",
			})
		}

		ctx := c.UserContext()
		cacheKey := "auth:apikey:" + apiKey

		// ==========================================
		// THE FAST PATH: Check Redis First
		// ==========================================
		cachedMerchant, err := redisClient.Get(ctx, cacheKey).Result()
		if err == nil {
			var merchant domain.Merchant
			if unmarshalErr := json.Unmarshal([]byte(cachedMerchant), &merchant); unmarshalErr == nil {
				// CACHE HIT! Set BOTH backpack variables to keep everything happy
				c.Locals("merchant", merchant)       // For the Rate Limiter
				c.Locals("merchant_id", merchant.ID) // For the Trust Handler
				return c.Next()
			}
		}

		// ==========================================
		// THE SLOW PATH: Query Postgres
		// ==========================================
		var merchant domain.Merchant
		err = pg.Where("api_key = ?", apiKey).First(&merchant).Error
		
		if err != nil {
			log.Printf("🚨 [AUTH] Blocked Invalid API Key from IP: %s", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Invalid API Key. Access Denied.",
			})
		}

		// ==========================================
		// CACHE POPULATION: Save for next time
		// ==========================================
		merchantJSON, _ := json.Marshal(merchant)
		// Save to Redis with a 5-Minute Time-To-Live (TTL)
		redisClient.Set(ctx, cacheKey, merchantJSON, 5*time.Minute)

		log.Printf("✅ [AUTH] Access granted to Store: %s", merchant.StoreName)

		// Set BOTH backpack variables
		c.Locals("merchant", merchant)       // For the Rate Limiter
		c.Locals("merchant_id", merchant.ID) // For the Trust Handler

		return c.Next()
	}
}
// RequireShopifyHMAC verifies incoming webhooks mathematically
// auth.go

// RequireShopifyHMAC now accepts the secret at startup to avoid runtime system calls
func RequireShopifyHMAC(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		shopifySignature := c.Get("X-Shopify-Hmac-Sha256")
		if shopifySignature == "" {
			log.Printf("🚨 [WEBHOOK] Blocked request missing HMAC signature from IP: %s", c.IP())
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
			log.Printf("🚨 [WEBHOOK] Cryptographic mismatch from IP: %s", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Cryptographic Mismatch",
			})
		}

		log.Printf("✅ [WEBHOOK] Shopify signature verified securely.")
		return c.Next()
	}
}