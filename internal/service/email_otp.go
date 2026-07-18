package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// GenerateOTP generates a 6-digit numeric OTP
func GenerateOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	return fmt.Sprintf("%06d", n.Int64()+100000)
}

// StoreOTP stores the OTP in Redis with a 10-minute TTL
// Key format: "email_otp:{email_normalized}" value: otp string
func StoreOTP(ctx context.Context, rdb *redis.Client, email, otp string) error {
	key := "email_otp:" + strings.ToLower(strings.TrimSpace(email))
	return rdb.Set(ctx, key, otp, 10*time.Minute).Err()
}

// VerifyOTP checks the OTP and deletes it on success (one-time use)
func VerifyOTP(ctx context.Context, rdb *redis.Client, email, otp string) (bool, error) {
	key := "email_otp:" + strings.ToLower(strings.TrimSpace(email))
	stored, err := rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return false, nil // expired or never set
	}
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(otp)) != 1 {
		return false, nil
	}
	rdb.Del(ctx, key) // one-time use — delete after successful verify
	return true, nil
}

// SendVerificationEmail logs the OTP to stdout for now and returns nil.
// TODO: wire to SendGrid/AWS SES before production
func SendVerificationEmail(email, otp string) error {
	slog.Info("Sending verification email", "email", email, "otp", otp)
	fmt.Printf("[Verification Email] To: %s | OTP: %s\n", email, otp)
	return nil
}
