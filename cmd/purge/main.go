// This binary is designed to run as an ECS Scheduled Task (EventBridge rule, daily cadence)
// running to completion and exiting. Do not build a cron loop inside this Go binary,
// as infrastructure scheduling (ECS/EventBridge) handles container lifecycle management properly.
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// retentionWindow represents the 30-day timeline before hard purging soft-deleted feedback logs.
// Confirm this against the privacy policy's stated retention period before modifying.
const retentionWindow = 30 * 24 * time.Hour

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting scheduled hard-purge job")

	pg := database.NewPostgresClient()

	cutoff := time.Now().Add(-retentionWindow)

	// Unscoped() is required here. Without it, GORM's default soft-delete
	// scope would silently skip rows that are already soft-deleted, which
	// is the exact opposite of what this purge job needs to target.
	result := pg.Unscoped().
		Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
		Delete(&domain.CustomerFeedback{})

	if result.Error != nil {
		slog.Error("hard purge failed", "error", result.Error)
		os.Exit(1)
	}

	slog.Info("hard purge complete", "rows_purged", result.RowsAffected, "cutoff", cutoff)

	result2 := pg.Model(&domain.BillableEvent{}).
		Where("created_at < ? AND raw_webhook_body != '' AND raw_webhook_body NOT LIKE '[REDACTED%'", cutoff).
		Update("raw_webhook_body", "[REDACTED-PURGE]")
	if result2.Error != nil {
		slog.Error("billable event redaction failed", "error", result2.Error)
		os.Exit(1)
	}
	slog.Info("billable event PII redacted", "rows_redacted", result2.RowsAffected)
}
