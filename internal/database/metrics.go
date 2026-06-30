package database

import (
	"log/slog"

	"gorm.io/gorm"
)

// IncrementMetric updates GORM database metrics atomically.
// It is database-agnostic and prevents SQL injection using an allowlist.
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

	result := db.Exec(query, phoneHash)
	if result.Error != nil {
		slog.Error("failed to update metrics in database", "error", result.Error, "hash", phoneHash[:8])
		return
	}
	if result.RowsAffected == 0 {
		slog.Error("metric upsert affected zero rows — possible race condition",
			"hash", phoneHash[:8],
			"column", columnName,
		)
	}
}
