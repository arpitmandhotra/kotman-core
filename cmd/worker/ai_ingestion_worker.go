package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type AIIngestionWorker struct {
	redis *redis.Client
	pg    *gorm.DB
}

func StartAIIngestionWorker(ctx context.Context, redisClient *redis.Client, postgresClient *gorm.DB) {
	slog.Info("Starting Kaughtman AI Ingestion Worker thread...")

	worker := &AIIngestionWorker{
		redis: redisClient,
		pg:    postgresClient,
	}

	worker.startConsuming(ctx)
}

func (w *AIIngestionWorker) startConsuming(ctx context.Context) {
	slog.Info("AI Ingestion Worker starting stream consumption...")

	// Initialize the stream and consumer group before starting consumption loop
	err := w.redis.XGroupCreateMkStream(ctx, "shadow_mode_ingestion", "ai_ingestion_group", "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		slog.Error("failed to create or verify Redis stream consumer group", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			streams, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
				Group:    "ai_ingestion_group",
				Consumer: "ai_ingestion_consumer",
				Streams:  []string{"shadow_mode_ingestion", ">"},
				Count:    10,
				Block:    2 * time.Second,
			}).Result()

			if err != nil {
				if err != redis.Nil {
					slog.Error("error reading from Redis stream shadow_mode_ingestion", "error", err)
					time.Sleep(1 * time.Second)
				}
				continue
			}

			for _, stream := range streams {
				for _, msg := range stream.Messages {
					w.processMessage(ctx, msg)
				}
			}
		}
	}
}

func (w *AIIngestionWorker) processMessage(ctx context.Context, msg redis.XMessage) {
	merchantIDVal, _ := msg.Values["merchant_id"].(string)
	payloadVal, _ := msg.Values["payload"].(string)

	if merchantIDVal == "" {
		slog.Warn("skipping message: missing merchant_id in stream message", "message_id", msg.ID)
		w.acknowledgeMessage(ctx, msg.ID)
		return
	}

	var merchant domain.Merchant
	err := w.pg.WithContext(ctx).
		Select("id", "is_active", "has_rto_engine", "store_name").
		Where("id = ?", merchantIDVal).
		First(&merchant).Error
	if err != nil {
		slog.Warn("merchant not found or database error matching ID", "message_id", msg.ID, "error", err)
		w.acknowledgeMessage(ctx, msg.ID)
		return
	}

	executionMode := domain.ExecutionModeActive

	// Parse payload flexibly to locate the Order ID
	var orderPayload struct {
		ID    json.Number `json:"id"`
		Order struct {
			ID          json.Number `json:"id"`
			IncrementID string      `json:"increment_id"`
		} `json:"order"`
		IncrementID string `json:"increment_id"`
	}

	var orderID string
	if payloadVal != "" {
		dec := json.NewDecoder(strings.NewReader(payloadVal))
		dec.UseNumber()
		if err := dec.Decode(&orderPayload); err == nil {
			if orderPayload.ID.String() != "" {
				orderID = orderPayload.ID.String()
			} else if orderPayload.Order.ID.String() != "" {
				orderID = orderPayload.Order.ID.String()
			} else if orderPayload.Order.IncrementID != "" {
				orderID = orderPayload.Order.IncrementID
			} else if orderPayload.IncrementID != "" {
				orderID = orderPayload.IncrementID
			}
		}
	}

	if orderID == "" {
		// Fallback order ID to prevent null/empty DB constraint issues
		orderID = "unknown_" + msg.ID
	}

	predictedRiskScore := mockRiskScore()

	audit := domain.OrderAudit{
		MerchantID:         merchant.ID,
		OrderID:            orderID,
		RawPayload:         payloadVal,
		PredictedRiskScore: predictedRiskScore,
		ExecutionMode:      executionMode,
	}

	if err := w.pg.WithContext(ctx).Create(&audit).Error; err != nil {
		slog.Error("failed to save order audit record to postgres", "message_id", msg.ID, "error", err)
		return
	}

	w.acknowledgeMessage(ctx, msg.ID)
}

func (w *AIIngestionWorker) acknowledgeMessage(ctx context.Context, messageID string) {
	err := w.redis.XAck(ctx, "shadow_mode_ingestion", "ai_ingestion_group", messageID).Err()
	if err != nil {
		slog.Error("failed to XACK message", "message_id", messageID, "error", err)
	}
}

// mockRiskScore generates a mock score between 0.0 and 100.0 using secure crypto/rand.
func mockRiskScore() float64 {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return 15.5 // fallback risk score
	}
	return float64(n.Int64()) / 100.0
}
