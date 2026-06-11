package database

import (
	"context"
	"log"
 "os"
	"github.com/redis/go-redis/v9"
)

// NewRedisClient creates and returns a connected Redis client.
func NewRedisClient() *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	var opt *redis.Options
	var err error

	if redisURL != "" {
		// Cloud: Parse the secure Upstash URL
		opt, err = redis.ParseURL(redisURL)
		if err != nil {
			log.Fatalf("Failed to parse Cloud Redis URL: %v", err)
		}
	} else {
		// Local: Fallback to your exact Docker configuration
		opt = &redis.Options{
			Addr:     "host.docker.internal:6379",
			Password: "",
			DB:       0,
		}
	}

	client := redis.NewClient(opt)

	_, err = client.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("--> Successfully connected to Redis Database!")
	return client
}