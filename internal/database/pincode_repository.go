package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type PincodeRepository struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewPincodeRepository(db *gorm.DB, rdb *redis.Client) *PincodeRepository {
	return &PincodeRepository{
		db:  db,
		rdb: rdb,
	}
}

// GetPincodeInfo returns geographic metadata for a pincode.
// Returns nil if the pincode is not found in the reference table.
// Results are cached in Redis with TTL 24h since this data changes rarely.
func (r *PincodeRepository) GetPincodeInfo(ctx context.Context, pincode string) (*domain.PincodeReference, error) {
	cacheKey := fmt.Sprintf("pincode:ref:%s", pincode)

	// Try cache first
	if r.rdb != nil {
		val, err := r.rdb.Get(ctx, cacheKey).Result()
		if err == nil && val != "" {
			var ref domain.PincodeReference
			if err := json.Unmarshal([]byte(val), &ref); err == nil {
				return &ref, nil
			}
		}
	}

	// Database lookup
	var ref domain.PincodeReference
	err := r.db.WithContext(ctx).Where("pincode = ?", pincode).First(&ref).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	// Cache result for 24 hours
	if r.rdb != nil {
		data, err := json.Marshal(ref)
		if err == nil {
			if err := r.rdb.Set(ctx, cacheKey, data, 24*time.Hour).Err(); err != nil {
				slog.Warn("failed to set pincode cache", "pincode", pincode, "error", err)
			}
		}
	}

	return &ref, nil
}
