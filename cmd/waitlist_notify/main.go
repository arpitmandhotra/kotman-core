// cmd/waitlist_notify/main.go
// This job is run ONCE manually when Growth tier launches.
// It fetches all waitlist entries and sends a launch notification email.
// Wire to SendGrid/AWS SES before running.

package main

import (
	"log/slog"
	"os"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

func main() {
	pg := database.NewPostgresClient()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	var entries []domain.WaitlistEntry
	if err := pg.Find(&entries).Error; err != nil {
		slog.Error("failed to fetch waitlist entries", "error", err)
		os.Exit(1)
	}

	slog.Info("waitlist entries to notify", "count", len(entries))
	for _, entry := range entries {
		slog.Info("would notify",
			"email", entry.Email,
			"store", entry.StoreName,
			"tier_interest", entry.TierInterest,
		)
		// TODO: replace above log with actual email send via SendGrid/SES
	}
	slog.Info("waitlist notification job complete (stub — no emails sent)")
}
