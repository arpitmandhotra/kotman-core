package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log"
	

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// RequireAPIKey checks the "X-API-Key" header against the Postgres database
func RequireAPIKey(pg *gorm.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Extract the key from the request header
		apiKey := c.Get("X-API-Key")

		// 2. If it's missing entirely, reject instantly (Zero database cost)
		if apiKey == "" {
			log.Printf("🚨 [AUTH] Blocked request missing API Key from IP: %s", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Missing X-API-Key header. Are you a registered merchant?",
			})
		}

		// 3. Look up the key in the Postgres Vault
		var merchant domain.Merchant
		err := pg.Where("api_key = ?", apiKey).First(&merchant).Error

		if err != nil {
			log.Printf("🚨 [AUTH] Blocked INVALID API Key from IP: %s", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"error":   "Invalid API Key. Access Denied.",
			})
		}

		// 4. Success! Let them through and log the store name
		log.Printf("🔓 [AUTH] Access granted to Store: %s", merchant.StoreName)
		
		// Optional: Attach the merchant ID to the context so your service can use it later
		c.Locals("merchant_id", merchant.ID)
		
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