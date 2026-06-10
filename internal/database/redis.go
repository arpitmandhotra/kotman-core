package database

import (
	"context"
	"log"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient creates and returns a connected Redis client.
func NewRedisClient() *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     "host.docker.internal:6379",
		Password: "",
		DB:       0,
	})

	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("--> Successfully connected to Redis Database!")
	return client
}
