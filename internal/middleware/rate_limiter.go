// internal/middleware/rate_limiter.go
package middleware

import (
    "log"
    "time"

    "github.com/gofiber/fiber/v2"
    "github.com/gofiber/fiber/v2/middleware/limiter"
    fiberstorage "github.com/gofiber/storage/redis/v3"
)

func SecurityBouncer(redisURL string) fiber.Handler {
    // Build the Redis storage backend for the limiter
    store := fiberstorage.New(fiberstorage.Config{
        URL:   redisURL,
        Reset: false, // don't wipe existing keys on startup
    })

    return limiter.New(limiter.Config{
        Max:        10,
        Expiration: 1 * time.Minute,
        Storage:    store, // ← this is the only real change

        KeyGenerator: func(c *fiber.Ctx) string {
            return "limiter:" + c.IP()
        },

        LimitReached: func(c *fiber.Ctx) error {
            ip := c.IP()
            log.Printf("🚨 [SECURITY] DDoS Deflected! Rate limit exceeded for IP: %s", ip)
            return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
                "success": false,
                "error":   "Too many requests. Please calm down and try again in a minute.",
            })
        },
    })
}