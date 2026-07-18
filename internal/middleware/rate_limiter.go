package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
	"context"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

func RequireRateLimit(redisClient *redis.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		merchant, ok := c.Locals("kaughtman.merchant").(domain.Merchant)
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
		randBytes := make([]byte, 8)
		rand.Read(randBytes)
		uniqueMember := fmt.Sprintf("%d-%s", now, hex.EncodeToString(randBytes))

		// ==========================================
		// ATOMIC LUA SCRIPT: Eliminates TOCTOU race
		// Redis executes Lua scripts atomically (single-threaded),
		// so count is checked AFTER adding the current request.
		// ==========================================
		luaScript := redis.NewScript(`
			redis.call('ZREMRANGEBYSCORE', KEYS[1], '0', ARGV[1])
			redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
			local count = redis.call('ZCARD', KEYS[1])
			redis.call('EXPIRE', KEYS[1], 60)
			return count
		`)

		result, err := luaScript.Run(ctx, redisClient,
			[]string{key},                        // KEYS[1]
			fmt.Sprintf("%d", windowStart),         // ARGV[1]
			float64(now),                           // ARGV[2]
			uniqueMember,                           // ARGV[3]
		).Int64()

		if err != nil {
			slog.Error("redis rate limit script failure",
				"error", err,
				"ip", c.IP(),
			)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "Security validation temporarily unavailable",
			})
		}

		// 5. Evaluate the Limit (count now includes the current request)
		currentCount := int(result)
		limit := 60 // default: 60 requests per minute per merchant
		if envLimit := os.Getenv("MERCHANT_RATE_LIMIT_PER_MINUTE"); envLimit != "" {
			if parsed, err := strconv.Atoi(envLimit); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		if currentCount > limit {
			slog.Warn("rate limit exceeded",
				"store",       merchant.StoreName,
				"merchant_id", merchant.ID,
				"count",       currentCount,
				"limit",       limit,
				"ip",          c.IP(),
			)
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Rate limit exceeded. Please slow down.",
			})
		}

		// Tell the merchant exactly when their 60-second window resets
		// currentCount already includes the current request (Lua adds before counting)
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

		// Let the request pass through to the next handler
		return c.Next()
	}
}

func RequireIPRateLimit(redisClient *redis.Client, limitPerMinute int) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ip := c.IP()
		key := "rate_limit:ip:" + ip

		now := time.Now().UnixNano()
		windowStart := now - (1 * time.Minute).Nanoseconds()

		ctx, cancel := context.WithTimeout(c.UserContext(), 45*time.Millisecond)
		defer cancel()

		randBytes := make([]byte, 8)
		rand.Read(randBytes)
		uniqueMember := fmt.Sprintf("%d-%s", now, hex.EncodeToString(randBytes))

		luaScript := redis.NewScript(`
			redis.call('ZREMRANGEBYSCORE', KEYS[1], '0', ARGV[1])
			redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
			local count = redis.call('ZCARD', KEYS[1])
			redis.call('EXPIRE', KEYS[1], 60)
			return count
		`)

		result, err := luaScript.Run(ctx, redisClient,
			[]string{key},
			fmt.Sprintf("%d", windowStart),
			float64(now),
			uniqueMember,
		).Int64()

		if err != nil {
			// M3 FIX: Fail CLOSED, not open. An attacker who can saturate/disconnect
			// Redis gets a free brute-force window if we fail-open here.
			slog.Error("ip rate limit redis error \u2014 failing closed", "ip", ip, "error", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "Security validation temporarily unavailable",
			})
		}

		if int(result) > limitPerMinute {
			slog.Warn("IP rate limit exceeded", "ip", ip, "count", result)
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "Too many requests from this IP",
			})
		}
		return c.Next()
	}
}