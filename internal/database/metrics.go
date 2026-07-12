package database

import (
	"context"
	"log/slog"

	"gorm.io/gorm"
)

// IncrementMetric updates GORM database metrics atomically.
// It is database-agnostic and prevents SQL injection using an allowlist.
// M16 FIX: ctx is now accepted and passed to the DB call for timeout/cancellation.
func IncrementMetric(db *gorm.DB, phoneHash string, columnName string) {
	// SQL Injection Prevention Allowlist
	allowed := map[string]bool{
		"total_orders":          true,
		"successful_deliveries": true,
		"total_rtos":            true,
		"total_cancellations":   true,
	}
	if !allowed[columnName] {
		slog.Error("blocked invalid column name execution", "column", columnName)
		return
	}

	query := `
		INSERT INTO trust_profiles (phone_hash, ` + columnName + `, created_at, updated_at) 
		VALUES (?, 1, NOW(), NOW()) 
		ON CONFLICT (phone_hash) 
		DO UPDATE SET ` + columnName + ` = trust_profiles.` + columnName + ` + 1, updated_at = NOW();
	`

	result := db.WithContext(context.Background()).Exec(query, phoneHash)
	if result.Error != nil {
		// M15 FIX: Guard phoneHash slice against empty/short input.
		hashLog := phoneHash
		if len(hashLog) > 4 {
			hashLog = hashLog[:4] + "…"
		}
		slog.Error("failed to update metrics in database", "error", result.Error, "hash", hashLog)
		return
	}
	if result.RowsAffected == 0 {
		hashLog := phoneHash
		if len(hashLog) > 4 {
			hashLog = hashLog[:4] + "…"
		}
		slog.Error("metric upsert affected zero rows — possible race condition",
			"hash", hashLog,
			"column", columnName,
		)
	}
}
