package service

import (
	"context"
	"log/slog"
	"strconv"
	"time"
    "gorm.io/gorm"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"golang.org/x/sync/singleflight"
	"github.com/redis/go-redis/v9"
)

type RedisTrustService struct {
	db *redis.Client
	pg    *gorm.DB
	requestGroup singleflight.Group
}

func NewRedisTrustService(client *redis.Client,pgClient *gorm.DB) *RedisTrustService {
	return &RedisTrustService{
		db: client,
	pg:  pgClient}
}

// EvaluateRisk now accepts the ipAddress to catch bots!
func (s *RedisTrustService) EvaluateRisk(ctx context.Context, phoneHash string, ipAddress string) (domain.TrustResponse, error) {

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
    "ip",       ipAddress,
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
	"score",      val, // val is the raw string from Redis, perfectly safe to log here
	"action",     "HIDE_COD",
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

		var record domain.BadActorRecord
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

	// 4. OBSERVABILITY: Did Singleflight save us from a stampede?
	if shared {
		slog.Info("singleflight database protected", "phone_hash", phoneHash[:8]+"...")
	}

	// 5. Build the final response based on what Singleflight returned
	finalScore := v.(int)
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
	record:= domain.BadActorRecord{
		PhoneHash: phoneHash,
		Reason: reason,
		LockedAt:time.Now(),
	}
	if err := s.pg.Create(&record).Error; err != nil {
		
slog.Error("failed to archive bad actor in postgres", "error", err, "phone_hash", phoneHash[:8]+"...")
		return err
	}
	slog.Info("bad actor archived in postgres", "phone_hash", phoneHash[:8]+"...")
	return nil
}
