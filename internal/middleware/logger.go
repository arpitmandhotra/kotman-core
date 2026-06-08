package middleware

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
)

// RequestLogger intercepts every HTTP request to measure speed and log traffic
func RequestLogger() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Start the stopwatch before the request hits the engine
		start := time.Now()

		// 2. Pass control to the next handler (e.g., your Webhook or Trust route)
		err := c.Next()

		// 3. Stop the stopwatch the exact millisecond the response is ready
		duration := time.Since(start)

		// 4. Print the forensic traffic log to your terminal
		log.Printf("[TRAFFIC] %s | %s | Status: %d | IP: %s | Latency: %v",
			c.Method(),
			c.Path(),
			c.Response().StatusCode(),
			c.IP(),
			duration,
		)

		return err
	}
}