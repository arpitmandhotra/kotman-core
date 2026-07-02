package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"os"

	"github.com/gofiber/fiber/v2"
)

// RequireAdminKey protects sensitive /v1/admin routes
func RequireAdminKey() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Fetch the master key from the server's secure environment
		expectedKey := os.Getenv("ADMIN_API_KEY")
		
		// FAILSAVE: If you forgot to set the env var on AWS, lock down the route completely.
		if expectedKey == "" {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "CRITICAL: Admin authentication is not configured on the server.",
			})
		}

		// 2. Grab the key the user sent in the HTTP headers
		providedKey := c.Get("X-Admin-Key")

		// Hash both keys using SHA-256 before constant-time comparison to prevent timing and length leaks
		expectedHash := sha256.Sum256([]byte(expectedKey))
		providedHash := sha256.Sum256([]byte(providedKey))

		// 3. Constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "Unauthorized. Invalid admin credentials.",
			})
		}

		// 4. The key is perfect. Let them through to the admin function.
		return c.Next()
	}
}