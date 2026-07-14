package courier

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/security"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type WebhookIngestionHandler struct {
	pg       *gorm.DB
	rdb      *redis.Client
	adapters map[CourierProvider]CourierWebhookAdapter
}

func NewWebhookIngestionHandler(pgDB *gorm.DB, redisClient *redis.Client) *WebhookIngestionHandler {
	adaps := map[CourierProvider]CourierWebhookAdapter{
		ProviderDelhivery:  NewDelhiveryAdapter(),
		ProviderShiprocket: NewShiprocketAdapter(),
		ProviderXpressbees: NewXpressbeesAdapter(),
		ProviderBluedart:   NewBluedartAdapter(),
		ProviderClickpost:  NewClickpostAdapter(),
	}
	return &WebhookIngestionHandler{
		pg:       pgDB,
		rdb:      redisClient,
		adapters: adaps,
	}
}

type IngestionQueueMsg struct {
	Provider CourierProvider `json:"provider"`
	RawBody  []byte          `json:"raw_body"`
	AWB      string          `json:"awb"`
}

// IngestCourierWebhook routes, validates, and queues incoming logistics webhooks.
// Route: POST /v1/webhooks/courier/:provider
func (h *WebhookIngestionHandler) IngestCourierWebhook(c *fiber.Ctx) error {
	providerParam := CourierProvider(strings.ToUpper(c.Params("provider")))
	adap, exists := h.adapters[providerParam]
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unsupported courier provider"})
	}

	// 1. Independent Ingestion Rate Limiter (protects endpoint against burst/DDoS)
	if err := h.rateLimitRequest(c, providerParam); err != nil {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "rate limit exceeded"})
	}

	rawBody := c.Body()
	if len(rawBody) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "empty body"})
	}

	// 2. Tenant Resolution (NEVER trust payload-claimed merchant identifiers!)
	awb, err := extractAWBFromRawPayload(providerParam, rawBody)
	if err != nil {
		slog.Warn("tenant resolution failed: unable to extract AWB from raw body", "provider", providerParam, "error", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid payload mapping"})
	}

	ctx := c.UserContext()

	// Resolve the tenant (MerchantID + OrderID) strictly via AWBMapping
	var mapping AWBMapping
	err = h.pg.WithContext(ctx).Where("awb = ?", awb).First(&mapping).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			slog.Warn("tenant resolution failed: AWB mapping not registered at label creation", "awb", awb, "provider", providerParam)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized tracking shipment"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "database resolution error"})
	}

	// Check if this merchant has NDR ingestion feature flagged
	if !h.isFeatureFlagged(ctx, mapping.MerchantID) {
		slog.Warn("NDR ingestion skipped: merchant feature flag not active", "merchant_id", mapping.MerchantID)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "feature flag disabled"})
	}

	// 3. Retrieve and Decrypt courier API credentials
	var creds domain.PlatformCredential
	err = h.pg.WithContext(ctx).
		Where("merchant_id = ? AND platform = ? AND is_active = true", mapping.MerchantID.String(), string(providerParam)).
		First(&creds).Error

	if err != nil {
		slog.Error("failed to retrieve merchant courier credentials", "merchant_id", mapping.MerchantID, "provider", providerParam)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing courier configuration"})
	}

	masterKeyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		slog.Error("TOKEN_ENCRYPTION_KEY environment variable is not set")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal configuration error"})
	}

	masterKeyBytes, err := base64.StdEncoding.DecodeString(masterKeyStr)
	if err != nil {
		slog.Error("invalid base64 token encryption key")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal configuration error"})
	}

	decryptedSecret, err := security.DecryptString(creds.WebhookSecretEncrypted, masterKeyBytes)
	if err != nil {
		decryptedSecret = creds.WebhookSecretEncrypted
	}

	// 4. Verify Signature (Cryptographically fail closed)
	req, err := adaptFiberRequest(c)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "request context error"})
	}

	err = adap.VerifySignature(rawBody, req.Header, decryptedSecret)
	if err != nil {
		slog.Warn("signature verification failed", "provider", providerParam, "awb", awb, "error", err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature verification"})
	}

	// 5. Ingest into Redis Queue (Bypass request thread within 10ms)
	queueMsg := IngestionQueueMsg{
		Provider: providerParam,
		RawBody:  rawBody,
		AWB:      awb,
	}
	msgBytes, err := json.Marshal(queueMsg)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "payload serialization error"})
	}

	err = h.rdb.LPush(ctx, "kaughtman:ndr_queue", string(msgBytes)).Err()
	if err != nil {
		slog.Error("failed to push to Redis queue", "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "queuing failure"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "webhook parsed and queued successfully",
	})
}

// rateLimitRequest rates limits requests on public webhooks to 100 req/min per provider/IP
func (h *WebhookIngestionHandler) rateLimitRequest(c *fiber.Ctx, provider CourierProvider) error {
	ctx := c.UserContext()
	key := fmt.Sprintf("ratelimit:webhook:%s:%s", string(provider), c.IP())
	val, err := h.rdb.Incr(ctx, key).Result()
	if err != nil {
		return nil // Fail open on Redis connection errors
	}
	if val == 1 {
		h.rdb.Expire(ctx, key, 1*time.Minute)
	}
	if val > 100 {
		return errors.New("rate limit exceeded")
	}
	return nil
}

// isFeatureFlagged verifies if a merchant is enrolled in the anchor brand pilot list
func (h *WebhookIngestionHandler) isFeatureFlagged(ctx context.Context, merchantID uuid.UUID) bool {
	var merchant domain.Merchant
	err := h.pg.WithContext(ctx).Where("id = ?", merchantID.String()).First(&merchant).Error
	if err != nil {
		return false
	}
	return merchant.HasRTOEngine
}

func extractAWBFromRawPayload(provider CourierProvider, body []byte) (string, error) {
	var payload struct {
		Waybill string `json:"waybill"`  // Delhivery
		AWBCode string `json:"awb_code"` // Shiprocket
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if provider == ProviderDelhivery && payload.Waybill != "" {
		return payload.Waybill, nil
	}
	if provider == ProviderShiprocket && payload.AWBCode != "" {
		return payload.AWBCode, nil
	}
	return "", errors.New("unable to extract tracking AWB code")
}

func adaptFiberRequest(c *fiber.Ctx) (*http.Request, error) {
	req, err := http.NewRequest(c.Method(), c.OriginalURL(), nil)
	if err != nil {
		return nil, err
	}
	c.Request().Header.VisitAll(func(key, value []byte) {
		req.Header.Set(string(key), string(value))
	})
	return req, nil
}
