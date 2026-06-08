package middleware

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
)

// SecurityBouncer creates a rate limiter that tracks requests by IP address
func SecurityBouncer() fiber.Handler {
	return limiter.New(limiter.Config{
		// 1. The Rules: Max 10 requests every 1 minute
		Max:        10,
		Expiration: 1 * time.Minute,

		// 2. The Target: How do we identify the user? By their IP Address.
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},

		// 3. The Rejection: What happens when they hit 11 requests?
		LimitReached: func(c *fiber.Ctx) error {
			ip := c.IP()
			log.Printf("🚨 [SECURITY] DDoS Deflected! Rate limit exceeded for IP: %s", ip)
			
			// Return an HTTP 429 status code instantly
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Too many requests. Please calm down and try again in a minute.",
			})
		},
	})
}