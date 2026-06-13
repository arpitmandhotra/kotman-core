package middleware

import (
	"fmt"
	"log"
	"time"
        "context"
		"math/rand"
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

		now := time.Now().UnixNano()
		
		// 2. Calculate exactly 60 seconds ago
		windowStart := now - (1 * time.Minute).Nanoseconds()

		// ⏱️ Start the 45ms countdown timer for the Redis Pipeline
		ctx, cancel := context.WithTimeout(c.UserContext(), 45*time.Millisecond)
		defer cancel() 

		// ==========================================
		// FIX 2: Prevent ZAdd Nanosecond Collisions
		// ==========================================
		uniqueMember := fmt.Sprintf("%d-%d", now, rand.Int63())

		// 3. ATOMIC PIPELINE
		pipe := redisClient.TxPipeline() 
		pipe.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", windowStart))
		countCmd := pipe.ZCard(ctx, key)
		
		// Drop the unique member into the queue
		pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: uniqueMember})
		pipe.Expire(ctx, key, 1*time.Minute)

		// 4. Execute the pipeline
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("🚨 [REDIS] Pipeline failure or timeout: %v", err)
			return c.Next() 
		}

		// 5. Evaluate the Limit
		currentCount := int(countCmd.Val())
		limit := 10 // Temporary hardcode, will move to config later

		// ==========================================
		// FIX 1: The Off-By-One Boundary
		// ==========================================
		if currentCount >= limit {
			log.Printf("🛑 [RATE LIMIT] Blocked Store: %s | Count: %d/%d", merchant.StoreName, currentCount+1, limit)
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Rate limit exceeded. Please slow down.",
			})
		}

		// Tell the merchant exactly when their 60-second window resets
	// ==========================================
		// FIX 3: Calculate Remaining mathematically
		// ==========================================
		// currentCount is what was in the DB before this request, so we subtract (currentCount + 1)
		remaining := limit - (currentCount + 1)
		if remaining < 0 {
			remaining = 0
		}

		// Tell the merchant exactly when their 60-second window resets
		resetTime := (now / int64(time.Second)) + 60

		// Set the standardized HTTP headers on the response
		c.Set("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
		c.Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
		c.Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetTime))

		// Let the request pass through to the next handler
		return c.Next()
	}
}