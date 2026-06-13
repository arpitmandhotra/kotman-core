package middleware

import (
	"fmt"
	"log"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

func RequireRateLimit(redisClient *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		merchant, ok := c.Locals("merchant").(domain.Merchant)
		if !ok {
			return c.Next() // Fail-open
		}

		key := "rate_limit:merchant:" + merchant.ID

		// 1. Get the current time down to the exact nanosecond
		now := time.Now().UnixNano()
		
		// 2. Calculate exactly 60 seconds ago
		windowStart := now - (1 * time.Minute).Nanoseconds()

		// 3. ATOMIC PIPELINE: Do all Redis math in one single network trip
		pipe := redisClient.Pipeline()

		// Step A: Sweep away any requests older than 60 seconds
		pipe.ZRemRangeByScore(c.UserContext(), key, "0", fmt.Sprintf("%d", windowStart))
		
		// Step B: Count how many requests are left in the 60-second window
		countCmd := pipe.ZCard(c.UserContext(), key)
		
		// Step C: Drop the new request into the queue with its exact timestamp
		pipe.ZAdd(c.UserContext(), key, redis.Z{Score: float64(now), Member: now})
		
		// Step D: Reset the self-destruct timer so inactive queues clear out of memory
		pipe.Expire(c.UserContext(), key, 1*time.Minute)
// 4. Execute the pipeline
		if _, err := pipe.Exec(c.UserContext()); err != nil {
			log.Printf("🚨 [REDIS] Pipeline failure: %v", err)
			return c.Next() // Fail-open
		}

		// === 🚀 THE NEW HEADER LOGIC ===
		// Convert the Redis ZCard count to an integer
		currentCount := int(countCmd.Val())
		limit := 10
		remaining := limit - currentCount
		if remaining < 0 {
			remaining = 0
		}

		// Tell the merchant exactly when their 60-second window resets
		resetTime := (now / int64(time.Second)) + 60

		// Set the standardized HTTP headers on the response
		c.Set("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
		c.Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
		c.Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetTime))
		// ===============================

		// 5. The Block 
		if currentCount > limit {
			log.Printf("🚨 [SECURITY] Sliding Window limit exceeded for Store: %s", merchant.StoreName)
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Too many requests. Please calm down and try again in a minute.",
			})
		}

		return c.Next()

	}
}