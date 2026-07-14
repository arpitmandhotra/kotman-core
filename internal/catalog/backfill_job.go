package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type CatalogBackfillJob struct {
	pg            *gorm.DB
	rdb           *redis.Client
	shopifyClient *ShopifySyncClient
}

func NewCatalogBackfillJob(pgDB *gorm.DB, redisClient *redis.Client) *CatalogBackfillJob {
	return &CatalogBackfillJob{
		pg:            pgDB,
		rdb:           redisClient,
		shopifyClient: NewShopifySyncClient(pgDB),
	}
}

type BackfillState struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	PageCursor string    `json:"page_cursor"`
	Status     string    `json:"status"` // "pending", "processing", "completed", "failed"
	UpdatedAt  time.Time `json:"updated_at"`
}

// TriggerBackfill triggers the async onboarding backfill job.
func (j *CatalogBackfillJob) TriggerBackfill(ctx context.Context, merchantID uuid.UUID, shopURL string, accessToken string) {
	slog.Info("queueing catalog backfill job", "merchant_id", merchantID)
	go func() {
		// Isolated goroutine execution context
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		err := j.RunBackfill(bgCtx, merchantID, shopURL, accessToken)
		if err != nil {
			slog.Error("async catalog backfill execution failed", "merchant_id", merchantID, "error", err)
		}
	}()
}

// RunBackfill executes the backfill while supporting resumability from checkpoint.
func (j *CatalogBackfillJob) RunBackfill(ctx context.Context, merchantID uuid.UUID, shopURL string, accessToken string) error {
	checkpointKey := fmt.Sprintf("catalog:backfill:state:%s", merchantID.String())

	// 1. Check if a previous run has checkpoint state
	var state BackfillState
	stateRaw, err := j.rdb.Get(ctx, checkpointKey).Result()
	if err == nil {
		_ = json.Unmarshal([]byte(stateRaw), &state)
	}

	if state.Status == "completed" {
		slog.Info("catalog backfill already completed, skipping", "merchant_id", merchantID)
		return nil
	}

	// Set processing status
	state.MerchantID = merchantID
	state.Status = "processing"
	state.UpdatedAt = time.Now()
	stateBytes, _ := json.Marshal(state)
	j.rdb.Set(ctx, checkpointKey, string(stateBytes), 7*24*time.Hour)

	// 2. Perform backfill via Shopify Client (handles paginated REST or Bulk GraphQL)
	err = j.shopifyClient.FetchAndSyncCatalog(ctx, merchantID, shopURL, accessToken)
	if err != nil {
		state.Status = "failed"
		state.UpdatedAt = time.Now()
		failedBytes, _ := json.Marshal(state)
		j.rdb.Set(ctx, checkpointKey, string(failedBytes), 7*24*time.Hour)
		return err
	}

	// 3. Mark completed
	state.Status = "completed"
	state.UpdatedAt = time.Now()
	completedBytes, _ := json.Marshal(state)
	j.rdb.Set(ctx, checkpointKey, string(completedBytes), 7*24*time.Hour)

	slog.Info("resumable backfill completed successfully for merchant", "merchant_id", merchantID)
	return nil
}
