package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/ai"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting scheduled merchant scores computation worker")

	// Startup validation of ANTHROPIC_API_KEY
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		slog.Warn("ANTHROPIC_API_KEY is not set. AI pipeline will degrade gracefully (AI insights will not be generated).")
	}

	pg := database.NewPostgresClient()

	scoreSvc := service.NewScoreService(pg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	err := scoreSvc.ComputeAllMerchantScores(ctx)
	if err != nil {
		slog.Error("merchant score computation worker failed", "error", err)
		os.Exit(1)
	}

	// After score computation, send to AI pipeline for insight generation
	if apiKey != "" {
		aiPayloads := scoreSvc.BuildAIPayloads(ctx)
		for _, payload := range aiPayloads {
			merchantCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			insights, err := ai.SendToAI(merchantCtx, payload)
			cancel()
			if err != nil {
				slog.Error("AI pipeline failed for merchant", "merchant_id", payload.MerchantID, "error", err)
				continue
			}
			scoreSvc.SaveAIInsights(ctx, payload.MerchantID, insights)
		}
	}

	slog.Info("merchant scores computation completed successfully")
}
