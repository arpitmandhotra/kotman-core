package courier

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/security"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type NDRProcessor struct {
	pg         *gorm.DB
	rdb        *redis.Client
	adapters   map[CourierProvider]CourierWebhookAdapter
	numWorkers int
	stopChan   chan struct{}
}

func NewNDRProcessor(pgDB *gorm.DB, redisClient *redis.Client, numWorkers int) *NDRProcessor {
	adaps := map[CourierProvider]CourierWebhookAdapter{
		ProviderDelhivery:  NewDelhiveryAdapter(),
		ProviderShiprocket: NewShiprocketAdapter(),
		ProviderXpressbees: NewXpressbeesAdapter(),
		ProviderBluedart:   NewBluedartAdapter(),
		ProviderClickpost:  NewClickpostAdapter(),
	}
	return &NDRProcessor{
		pg:         pgDB,
		rdb:        redisClient,
		adapters:   adaps,
		numWorkers: numWorkers,
		stopChan:   make(chan struct{}),
	}
}

func (p *NDRProcessor) Start(ctx context.Context) {
	slog.Info("starting logistics NDR normalize workers", "count", p.numWorkers)
	for i := 0; i < p.numWorkers; i++ {
		go p.workerLoop(ctx, i)
	}
}

func (p *NDRProcessor) Stop() {
	close(p.stopChan)
}

func (p *NDRProcessor) workerLoop(ctx context.Context, id int) {
	for {
		select {
		case <-p.stopChan:
			return
		case <-ctx.Done():
			return
		default:
			result, err := p.rdb.BRPop(ctx, 5*time.Second, "kaughtman:ndr_queue").Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					continue
				}
				slog.Error("worker failed popping queue", "worker_id", id, "error", err)
				time.Sleep(1 * time.Second)
				continue
			}

			if len(result) < 2 {
				continue
			}
			raw := result[1]

			var msg IngestionQueueMsg
			if err := json.Unmarshal([]byte(raw), &msg); err != nil {
				slog.Error("failed decoding queue message", "error", err)
				continue
			}

			if err := p.ProcessNormalizedEvent(ctx, msg); err != nil {
				slog.Error("failed processing delivery event", "awb", msg.AWB, "error", err)
			}
		}
	}
}

func (p *NDRProcessor) ProcessNormalizedEvent(ctx context.Context, msg IngestionQueueMsg) error {
	adap, exists := p.adapters[msg.Provider]
	if !exists {
		return fmt.Errorf("unsupported provider: %s", msg.Provider)
	}

	rawEvent, err := adap.ParseEvent(msg.RawBody)
	if err != nil {
		return fmt.Errorf("failed parsing raw payload: %w", err)
	}

	event, err := adap.Normalize(rawEvent)
	if err != nil {
		return fmt.Errorf("failed normalizing payload details: %w", err)
	}

	// tenant resolution strictly via mapping
	var mapping AWBMapping
	err = p.pg.WithContext(ctx).Where("awb = ?", event.AWB).First(&mapping).Error
	if err != nil {
		return fmt.Errorf("failed resolving tenant for AWB %s: %w", event.AWB, err)
	}

	event.MerchantID = mapping.MerchantID
	event.OrderID = mapping.OrderID

	eventHash := securityHash(event.AWB, string(event.EventType), event.CourierTimestamp, string(event.CourierProvider))
	idempotencyKey := fmt.Sprintf("idempotency:ndr:%s", eventHash)

	isUnique, err := p.rdb.SetNX(ctx, idempotencyKey, "locked", 24*time.Hour).Result()
	if err != nil {
		return fmt.Errorf("failed checking Redis idempotency: %w", err)
	}
	if !isUnique {
		slog.Info("dropped duplicate event (redis idempotency hit)", "awb", event.AWB)
		return nil
	}

	var processed ProcessedWebhookEvent
	err = p.pg.WithContext(ctx).Where("event_hash = ?", eventHash).First(&processed).Error
	if err == nil {
		slog.Info("dropped duplicate event (database idempotency hit)", "awb", event.AWB)
		return nil
	}

	masterKeyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		p.rdb.Del(ctx, idempotencyKey)
		return errors.New("master encryption key not configured")
	}
	masterKeyBytes, err := base64.StdEncoding.DecodeString(masterKeyStr)
	if err != nil {
		p.rdb.Del(ctx, idempotencyKey)
		return errors.New("invalid base64 encryption key")
	}

	encryptedPayload, err := security.EncryptString(string(msg.RawBody), masterKeyBytes)
	if err != nil {
		p.rdb.Del(ctx, idempotencyKey)
		return fmt.Errorf("failed encrypting raw payload: %w", err)
	}
	event.RawPayloadEncrypted = []byte(encryptedPayload)

	err = p.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var currentEvent NormalizedDeliveryEvent
		err = tx.Where("awb = ?", event.AWB).Order("courier_timestamp DESC").First(&currentEvent).Error
		if err == nil {
			if event.CourierTimestamp.Before(currentEvent.CourierTimestamp) {
				slog.Info("out of order event ignored (newer state exists)", "awb", event.AWB)
				return nil
			}
			if currentEvent.EventType == EventDelivered || currentEvent.EventType == EventRTODelivered {
				slog.Info("ignore status change: package already at terminal delivery state", "awb", event.AWB)
				return nil
			}
		}

		err = tx.Create(&ProcessedWebhookEvent{
			EventHash: eventHash,
		}).Error
		if err != nil {
			return fmt.Errorf("idempotency block: duplicate transaction: %w", err)
		}

		if err := tx.Create(&event).Error; err != nil {
			return fmt.Errorf("failed saving normalized delivery event: %w", err)
		}

		var billable domain.BillableEvent
		err = tx.Where("order_id = ? AND merchant_id = ?", event.OrderID.String(), event.MerchantID.String()).First(&billable).Error
		if err == nil {
			if event.EventType == EventRTOInitiated || event.EventType == EventRTODelivered {
				billable.IsRTO = true
				if err := tx.Save(&billable).Error; err != nil {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		p.rdb.Del(ctx, idempotencyKey)
		return err
	}

	return nil
}

func securityHash(awb, eventType string, t time.Time, provider string) string {
	raw := fmt.Sprintf("%s:%s:%d:%s", awb, eventType, t.Unix(), provider)
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(raw))
	return hex.EncodeToString(hasher.Sum(nil))
}
