package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting scheduled merchant scores computation worker")

	pg := database.NewPostgresClient()

	scoreSvc := service.NewScoreService(pg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	err := scoreSvc.ComputeAllMerchantScores(ctx)
	if err != nil {
		slog.Error("merchant score computation worker failed", "error", err)
		os.Exit(1)
	}

	slog.Info("merchant scores computation completed successfully")
}
