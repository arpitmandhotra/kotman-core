package middleware

import (
    "log/slog"
    "time"

    "github.com/gofiber/fiber/v2"
)

func RequestLogger(log *slog.Logger) fiber.Handler {
    return func(c *fiber.Ctx) error {
        start := time.Now()
        err   := c.Next()

        log.Info("request",
            "method",      c.Method(),
            "path",        c.Path(),
            "status",      c.Response().StatusCode(),
            "ip",          c.IP(),
            "latency_ms",  time.Since(start).Milliseconds(),
            "merchant_id", c.Locals("kotman.merchant_id"),
        )

        return err
    }
}