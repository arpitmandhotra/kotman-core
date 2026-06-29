package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

type RedisTrustService struct {
	db           *redis.Client
	pg           *gorm.DB
	requestGroup singleflight.Group
}

func NewRedisTrustService(client *redis.Client, pgClient *gorm.DB) *RedisTrustService {
	return &RedisTrustService{
		db: client,
		pg: pgClient}
}

// CalculateFee resolves the transaction fee strictly on the 'cart_value' bounds.
func CalculateFee(cartValue float64) float64 {
	switch {
	case cartValue <= 500:
		return 5.00
	case cartValue <= 1000:
		return 7.50
	case cartValue <= 2000:
		return 10.00
	case cartValue <= 3000:
		return 20.00
	case cartValue <= 4000:
		return 30.00
	case cartValue <= 5000:
		return 40.00
	case cartValue <= 10000:
		return 50.00
	default:
		return 100.00
	}
}

// EvaluateRisk now accepts the ipAddress to catch bots, merchantID, and cartValue!
func (s *RedisTrustService) EvaluateRisk(ctx context.Context, phoneHash string, ipAddress string, merchantID string, cartValue float64) (domain.TrustResponse, error) {

	// ==========================================
	// HEURISTIC 1: THE VELOCITY BOT CHECK (IP)
	// ==========================================
	velocityKey := "velocity_ip:" + ipAddress

	attempts, err := s.db.Incr(ctx, velocityKey).Result()
	if err != nil {
		slog.Error("redis velocity error", "error", err, "ip", ipAddress)
	}

	if attempts == 1 {
		s.db.Expire(ctx, velocityKey, 5*time.Minute)
	}

	if attempts > 3 {
		slog.Warn("velocity bot detected",
			"ip", ipAddress,
			"attempts", attempts,
		)
		return domain.TrustResponse{
			PhoneHash: phoneHash,
			Score:     10,
			Action:    "HIDE_COD",
		}, nil
	}

	// ==========================================
	// BILLING: Value-Based Tiered Wallet Deduction
	// ==========================================
	fee := CalculateFee(cartValue)
	var updatedBalance float64

	err = s.pg.Transaction(func(tx *gorm.DB) error {
		// 1. Balance verification and subtraction
		// Checks RowsAffected == 0 to prevent race conditions (AGENTS.md rule)
		result := tx.Model(&domain.MerchantSettings{}).
			Where("merchant_id = ? AND wallet_balance >= ?", merchantID, fee).
			Update("wallet_balance", gorm.Expr("wallet_balance - ?", fee))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("insufficient wallet balance")
		}

		// 2. Insert into transaction history
		history := domain.TransactionHistory{
			MerchantID: merchantID,
			CartValue:  cartValue,
			FeeCharged: fee,
		}
		if err := tx.Create(&history).Error; err != nil {
			return err
		}

		// 3. Load updated balance for Redis cache sync
		var settings domain.MerchantSettings
		if err := tx.Where("merchant_id = ?", merchantID).First(&settings).Error; err != nil {
			return err
		}
		updatedBalance = settings.WalletBalance
		return nil
	})

	if err != nil {
		slog.Error("billing deduction failed", "merchant_id", merchantID, "error", err)
		return domain.TrustResponse{}, fmt.Errorf("billing deduction failed: %w", err)
	}

	// 4. Redis Cache Sync for local account balance
	balanceKey := "merchant:balance:" + merchantID
	if cacheErr := s.db.Set(ctx, balanceKey, fmt.Sprintf("%.2f", updatedBalance), 0).Err(); cacheErr != nil {
		slog.Error("failed to sync wallet balance to redis cache", "error", cacheErr, "merchant_id", merchantID)
	}

	// ==========================================
	// HEURISTIC 2: SINGLEFLIGHT POSTGRES VAULT
	// ==========================================

	// 1. THE FAST PATH (Check RAM)
	val, err := s.db.Get(ctx, phoneHash).Result()
	if err == nil { // Found in Redis!
		slog.Info("cache hit",
			"phone_hash", phoneHash[:8]+"...",
			"score", val, // val is the raw string from Redis, perfectly safe to log here
			"action", "HIDE_COD",
		)
		parsedScore, _ := strconv.Atoi(val)
		return domain.TrustResponse{
			PhoneHash: phoneHash,
			Score:     parsedScore,
			Action:    "HIDE_COD",
		}, nil
	}

	// 2. CACHE MISS! Enter the Singleflight waiting room.
	v, err, shared := s.requestGroup.Do(phoneHash, func() (interface{}, error) {
		slog.Info("cache miss querying postgres", "phone_hash", phoneHash[:8]+"...")

		var record domain.TrustProfile
		dbErr := s.pg.Where("phone_hash = ?", phoneHash).First(&record).Error

		// If it's not in Postgres either, the user is completely clean!
		if dbErr != nil {
			s.db.Set(ctx, phoneHash, "85", 15*time.Minute)
			return 85, nil // 85 = High Trust
		}

		// 3. CACHE WARMING!
		// We found them in Cold Storage. Copy them back to RAM for 24 hours.
		s.db.Set(ctx, phoneHash, "20", 24*time.Hour)

		return 20, nil // 20 = Low Trust
	})

	// Handle singleflight errors before touching the result value.
	if err != nil {
		slog.Error("singleflight lookup failed", "phone_hash", phoneHash[:8]+"...", "error", err)
		return domain.TrustResponse{}, err
	}

	// 4. OBSERVABILITY: Did Singleflight save us from a stampede?
	if shared {
		slog.Info("singleflight database protected", "phone_hash", phoneHash[:8]+"...")
	}

	// 5. Build the final response based on what Singleflight returned
	finalScore, ok := v.(int)
	if !ok {
		slog.Error("unexpected type from singleflight result", "phone_hash", phoneHash[:8]+"...")
		return domain.TrustResponse{}, fmt.Errorf("internal error: singleflight returned non-int type")
	}
	action := "ALLOW_COD"
	if finalScore <= 20 {
		action = "HIDE_COD"
	}

	return domain.TrustResponse{
		PhoneHash: phoneHash,
		Score:     finalScore,
		Action:    action,
	}, nil

}

func (s *RedisTrustService) ReportBadActor(ctx context.Context, phoneHash string, reason string) error {
	expirationTime := 24 * time.Hour * 180
	err := s.db.Set(ctx, phoneHash, "20", expirationTime).Err()
	if err != nil {
		slog.Error("failed to save bad actor to redis", "error", err, "phone_hash", phoneHash[:8]+"...")
		return err
	}
	slog.Info("bad actor saved to redis", "phone_hash", phoneHash[:8]+"...", "reason", reason)
	// 1. Create the time variable first so we can pass its pointer
		now := time.Now()

		record := domain.TrustProfile{
			PhoneHash:       phoneHash,
			IsBlacklisted:   true,     // Explicitly mark as a bad actor
			BlacklistReason: reason,   // Use the parameter passed into the function
			LockedAt:        &now,     // Pass the memory address pointer
		}
	if err := s.pg.Create(&record).Error; err != nil {

		slog.Error("failed to archive bad actor in postgres", "error", err, "phone_hash", phoneHash[:8]+"...")
		return err
	}
	slog.Info("bad actor archived in postgres", "phone_hash", phoneHash[:8]+"...")
	return nil
}
