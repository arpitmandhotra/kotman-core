package service

import (
	"context"
	"log"
	"strconv"
	"time"
    "gorm.io/gorm"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/redis/go-redis/v9"
)

type RedisTrustService struct {
	db *redis.Client
	pg    *gorm.DB
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
		log.Printf("Redis velocity error: %v", err)
	}

	if attempts == 1 {
		s.db.Expire(ctx, velocityKey, 5*time.Minute)
	}

	if attempts > 3 {
		log.Printf("🚨 VELOCITY BOT DETECTED! IP: %s attempted %d times.", ipAddress, attempts)
		return domain.TrustResponse{
			PhoneHash: phoneHash,
			Score:     10,
			Action:    "HIDE_COD",
		}, nil
	}

	// ==========================================
	// HEURISTIC 2: THE KNOWN SCAMMER CHECK (Hash)
	// ==========================================
	val, err := s.db.Get(ctx, phoneHash).Result()

	if err == redis.Nil {
		return domain.TrustResponse{
			PhoneHash: phoneHash,
			Score:     85,
			Action:    "ALLOW_COD",
		}, nil
	}

	if err != nil {
		return domain.TrustResponse{}, err
	}

	parsedScore, _ := strconv.Atoi(val)

	return domain.TrustResponse{
		PhoneHash: phoneHash,
		Score:     parsedScore,
		Action:    "HIDE_COD",
	}, nil

}

func (s *RedisTrustService) ReportBadActor(ctx context.Context, phoneHash string, reason string) error {
	expirationTime := 24 * time.Hour * 180
	err := s.db.Set(ctx, phoneHash, "20", expirationTime).Err()
	if err != nil {
		log.Printf("Failed to save the bad actor to redis: %v", err)
		return err
	}
	log.Printf("Succesfuly saved the bad actor %s to redis because : %s", phoneHash, reason)
	record:= domain.BadActorRecord{
		PhoneHash: phoneHash,
		Reason: reason,
		LockedAt:time.Now(),
	}
	if err := s.pg.Create(&record).Error; err != nil {
		log.Printf("Failed to archive bad actor in Postgres: %v", err)
		return err
	}
	log.Printf("--> [DISK] Scammer permanently archived in Cold Storage.")

	return nil
}
