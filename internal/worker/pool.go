package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/webhooks"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// NDRWorkerPool manages concurrent goroutines processing normalized logistics webhooks.
type NDRWorkerPool struct {
	pg       *gorm.DB
	rdb      *redis.Client
	handlers map[string]webhooks.CarrierWebhookHandler
	numWorkers int
	stopChan   chan struct{}
}

func NewNDRWorkerPool(pgDB *gorm.DB, redisClient *redis.Client, numWorkers int) *NDRWorkerPool {
	hs := map[string]webhooks.CarrierWebhookHandler{
		"delhivery":  webhooks.NewDelhiveryHandler(),
		"shiprocket": webhooks.NewShiprocketHandler(),
	}
	return &NDRWorkerPool{
		pg:         pgDB,
		rdb:        redisClient,
		handlers:   hs,
		numWorkers: numWorkers,
		stopChan:   make(chan struct{}),
	}
}

// Start launches the background goroutines in the worker pool.
func (p *NDRWorkerPool) Start(ctx context.Context) {
	slog.Info("starting logistics NDR webhook worker pool", "workers_count", p.numWorkers)
	for i := 0; i < p.numWorkers; i++ {
		go p.worker(ctx, i)
	}
}

// Stop signals all workers to terminate gracefully.
func (p *NDRWorkerPool) Stop() {
	slog.Info("stopping worker pool...")
	close(p.stopChan)
}

func (p *NDRWorkerPool) worker(ctx context.Context, workerID int) {
	for {
		select {
		case <-p.stopChan:
			slog.Info("worker exiting", "worker_id", workerID)
			return
		case <-ctx.Done():
			slog.Info("worker context cancelled, exiting", "worker_id", workerID)
			return
		default:
			// Low-latency blocking pop from Redis queue (timeout: 5 seconds to allow loop checks)
			result, err := p.rdb.BRPop(ctx, 5*time.Second, "kaughtman:ndr_queue").Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					continue // Queue is empty, keep waiting
				}
				slog.Error("worker queue pop failed", "worker_id", workerID, "error", err)
				time.Sleep(1 * time.Second)
				continue
			}

			// BRPop returns a slice: [key, value]
			if len(result) < 2 {
				continue
			}
			rawMsg := result[1]

			var msg webhooks.QueueMessage
			if err := json.Unmarshal([]byte(rawMsg), &msg); err != nil {
				slog.Error("failed to parse queued message", "worker_id", workerID, "error", err)
				continue
			}

			err = p.processNDRMessage(ctx, msg)
			if err != nil {
				slog.Error("NDR message processing failed", "carrier", msg.CarrierName, "error", err)
			}
		}
	}
}

func (p *NDRWorkerPool) processNDRMessage(ctx context.Context, msg webhooks.QueueMessage) error {
	handler, exists := p.handlers[msg.CarrierName]
	if !exists {
		return fmt.Errorf("unsupported carrier: %s", msg.CarrierName)
	}

	// 1. Dynamic strategy parsing & normalization
	event, err := handler.ParsePayload(ctx, msg.RawBody)
	if err != nil {
		return fmt.Errorf("failed to parse payload: %w", err)
	}

	// 2. Strict Idempotency check via Redis SETNX
	// Key: idempotency:webhook:{carrier_name}:{tracking_awb}:{status_code}
	// 24 hour TTL to prevent duplicate processing on logistics partner retries.
	idempotencyKey := fmt.Sprintf("idempotency:webhook:%s:%s:%s", event.CarrierName, event.TrackingAWB, event.StatusCode)
	isUnique, err := p.rdb.SetNX(ctx, idempotencyKey, "processed", 24*time.Hour).Result()
	if err != nil {
		return fmt.Errorf("failed to check idempotency in Redis: %w", err)
	}
	if !isUnique {
		slog.Info("dropped duplicate webhook event (idempotency match)", "key", idempotencyKey)
		return nil
	}

	// 3. PostgreSQL Transaction to update fulfillment logs and order state
	err = p.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Log the delivery event exception
		logRecord := domain.NDRFulfillmentLog{
			TrackingAWB:     event.TrackingAWB,
			CarrierName:     event.CarrierName,
			InternalOrderID: event.InternalOrderID,
			StatusCode:      event.StatusCode,
			NDRReason:       event.NDRReason,
			AttemptCount:    event.AttemptCount,
			EventTimestamp:  event.EventTimestamp,
		}

		if err := tx.Create(&logRecord).Error; err != nil {
			return fmt.Errorf("failed to record NDR fulfillment log: %w", err)
		}

		// Update order details if order matches AWB or ID
		// In a production P&L system, NDR impacts delivered status and shipping charges.
		var order domain.Order
		err := tx.Where("tracking_awb = ? OR id = ?", event.TrackingAWB, event.InternalOrderID).First(&order).Error
		if err == nil {
			// Update status of order
			order.DeliveryStatus = "NDR_" + event.StatusCode
			order.NDRAttempts = event.AttemptCount
			if err := tx.Save(&order).Error; err != nil {
				return fmt.Errorf("failed to update order delivery status: %w", err)
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("failed to query matching order: %w", err)
		}

		return nil
	})

	if err != nil {
		// Cleanup Redis idempotency key on db transaction failure to allow retry processing
		p.rdb.Del(ctx, idempotencyKey)
		return err
	}

	slog.Info("successfully processed normalized NDR event", "awb", event.TrackingAWB, "carrier", event.CarrierName)
	return nil
}
