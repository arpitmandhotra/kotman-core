package webhooks

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/security"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// WebhookIngestionHandler orchestrates low-latency webhook validation and queuing.
type WebhookIngestionHandler struct {
	pg       *gorm.DB
	rdb      *redis.Client
	handlers map[string]CarrierWebhookHandler
}

func NewWebhookIngestionHandler(pgDB *gorm.DB, redisClient *redis.Client) *WebhookIngestionHandler {
	hs := map[string]CarrierWebhookHandler{
		"delhivery":  NewDelhiveryHandler(),
		"shiprocket": NewShiprocketHandler(),
	}
	return &WebhookIngestionHandler{
		pg:       pgDB,
		rdb:      redisClient,
		handlers: hs,
	}
}

// QueueMessage models the payload pushed to Redis for asynchronous worker consumption.
type QueueMessage struct {
	CarrierName string `json:"carrier_name"`
	RawBody     []byte `json:"raw_body"`
}

// IngestCarrierWebhook handles public HTTP requests from logistics partners.
// Route: POST /v1/webhooks/carrier/:carrier
func (h *WebhookIngestionHandler) IngestCarrierWebhook(c *fiber.Ctx) error {
	carrier := c.Params("carrier")
	handler, exists := h.handlers[carrier]
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "unsupported logistics carrier",
		})
	}

	rawBody := c.Body()
	if len(rawBody) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "empty request body",
		})
	}

	// Retrieve merchant context / credentials to validate the signature
	merchantID := c.Query("merchant_id")
	if merchantID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing merchant_id parameter",
		})
	}

	ctx := c.UserContext()

	// 1. Fetch courier credentials from database
	var creds domain.PlatformCredential
	err := h.pg.WithContext(ctx).
		Where("merchant_id = ? AND platform = ? AND is_active = true", merchantID, carrier).
		First(&creds).Error

	if err != nil {
		slog.Error("webhook ingestion: merchant credentials not found", "merchant_id", merchantID, "carrier", carrier)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "unauthorized webhook source",
		})
	}

	// 2. Decrypt the secret key stored in the Postgres database
	masterKeyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		slog.Error("webhook ingestion: master key missing from environment")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal configuration error",
		})
	}

	masterKeyBytes, err := base64.StdEncoding.DecodeString(masterKeyStr)
	if err != nil {
		slog.Error("webhook ingestion: invalid base64 master key")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "internal configuration error",
		})
	}

	decrypted, err := security.DecryptString(creds.WebhookSecretEncrypted, masterKeyBytes)
	var decryptedSecret []byte
	if err == nil {
		decryptedSecret = []byte(decrypted)
	} else {
		// Fallback to raw string if decryption fails or was not encrypted
		decryptedSecret = []byte(creds.WebhookSecretEncrypted)
	}

	// Convert Fiber request to net/http request for strategy validators
	req, err := adaptFiberRequest(c)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to process request headers",
		})
	}

	// 3. Cryptographically validate the signature
	err = handler.ValidateSignature(ctx, req, rawBody, decryptedSecret)
	if err != nil {
		slog.Warn("webhook signature validation failed", "carrier", carrier, "error", err)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "invalid webhook signature",
		})
	}

	// 4. Ingest and queue raw payload immediately to prevent Postgres thread exhaustion
	queueMsg := QueueMessage{
		CarrierName: carrier,
		RawBody:     rawBody,
	}
	msgBytes, err := json.Marshal(queueMsg)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to serialize message for queue",
		})
	}

	// Push to Redis list/queue (Low latency, typically < 10ms)
	err = h.rdb.LPush(ctx, "kaughtman:ndr_queue", string(msgBytes)).Err()
	if err != nil {
		slog.Error("failed to push webhook to Redis queue", "carrier", carrier, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "temporary ingestion failure",
		})
	}

	// Return 202 Accepted to release the carrier HTTP thread immediately (within SLA limits)
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"success": true,
		"message": "webhook received and queued for processing",
	})
}

// adaptFiberRequest maps headers from Fiber context into an *http.Request for strategy validators
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
