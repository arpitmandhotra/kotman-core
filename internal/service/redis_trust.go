package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
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

	velocityScript := redis.NewScript(`
		local count = redis.call('INCR', KEYS[1])
		redis.call('EXPIRE', KEYS[1], ARGV[1], 'NX')
		return count
	`)
	attempts, err := velocityScript.Run(ctx, s.db, []string{velocityKey}, "300").Int64()
	if err != nil {
		slog.Error("redis velocity error", "error", err, "ip", ipAddress)
		attempts = 0
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

		if dbErr != nil {
			// New phone — no history. Default high trust, short cache.
			s.db.Set(ctx, phoneHash, "85", 15*time.Minute)
			return 85, nil
		}

		// Compute a real score from profile data
		features := record.GenerateAIFeatures(0)
		score := 100.0

		// Apply RTO rate penalty (biggest signal)
		if rtoRate, ok := features["network_rto_rate"].(float64); ok {
			score -= rtoRate * 60 // 100% RTO rate = -60 points
		}

		// Apply cancellation frequency penalty
		if cancelRate, ok := features["cancellation_frequency"].(float64); ok {
			score -= cancelRate * 20 // 100% cancel rate = -20 points
		}

		// Apply accumulated risk adjustment from feedback
		if riskAdj, ok := features["risk_adjustment"].(float64); ok {
			score += riskAdj // negative deltas from FRAUD_SUSPECTED etc.
		}

		// Apply blacklist override
		if record.IsBlacklisted {
			score = 5
		}

		// Clamp to [0, 100]
		if score < 0 { score = 0 }
		if score > 100 { score = 100 }

		finalScore := int(math.Round(score))
		cacheVal := fmt.Sprintf("%d", finalScore)
		cacheTTL := 24 * time.Hour
		if finalScore < 30 {
			cacheTTL = 1 * time.Hour // re-evaluate bad actors more frequently
		}
		s.db.Set(ctx, phoneHash, cacheVal, cacheTTL)
		return finalScore, nil
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
	if finalScore < 40 {
		action = "HIDE_COD"
	} else if finalScore < 60 {
		action = "REQUIRE_VERIFICATION" // future: trigger WhatsApp OTP
	} else {
		action = "ALLOW_COD"
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
