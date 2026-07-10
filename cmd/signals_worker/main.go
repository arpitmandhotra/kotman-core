// This binary is designed to run as an ECS Scheduled Task (EventBridge rule, cron: 0 2 * * *)
// running to completion and exiting. Do not build a cron loop inside this Go binary,
// as infrastructure scheduling (ECS/EventBridge) handles container lifecycle management properly.
package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

// kAnonymityThreshold is the minimum number of distinct merchants required
// for a cohort cell to be included in signal outputs. This is a privacy-
// preserving guard that prevents individual merchant behaviour from being
// reverse-engineered from aggregate data. NON-NEGOTIABLE.
const kAnonymityThreshold = 5

// AggregationRow holds one row from the GROUP BY aggregation query
// against public.billable_events.
type AggregationRow struct {
	CategoryL1         string
	CategoryL2         string
	GeoState           string
	GeoTier            int
	OrderCount         int
	MerchantCount      int
	TotalGMVPaise      int64
	RTORate            float64
	CODShare           float64
	AvgOrderValuePaise float64
}

type GeoAggregationRow struct {
	GeoState      string
	GeoTier       int
	OrderCount    int
	MerchantCount int
	TotalGMVPaise int64
	RTORate       float64
	CODShare      float64
}

type PaymentAggregationRow struct {
	CategoryL1    string
	GeoState      string
	MerchantCount int
	CODShare      float64
}

// IndexBasePeriod represents a row in signals.index_base_periods.
type IndexBasePeriod struct {
	CohortKey string
	BaseGMV   float64
	BaseAOV   float64
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting signals aggregation worker")

	pg := database.NewPostgresClient()

	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		slog.Error("failed to load IST timezone", "error", err)
		os.Exit(1)
	}

	// snapshot_date is "today" in IST — the end boundary of every window.
	now := time.Now().In(ist)
	snapshotDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)

	windows := []int{1, 2}
	if os.Getenv("SIGNALS_ENABLE_7DAY_WINDOW") == "true" {
		windows = append(windows, 7)
	}

	for _, w := range windows {
		if err := RunAggregation(pg, snapshotDate, w); err != nil {
			slog.Error("aggregation failed", "window_days", w, "error", err)
			os.Exit(1)
		}
	}

	slog.Info("signals aggregation worker completed successfully")
}

// countActiveMerchants checks the count of distinct active merchants in the last 7 days.
func countActiveMerchants(db *gorm.DB) (int, error) {
	var count int
	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour)
	err := db.Raw(`
		SELECT COUNT(DISTINCT merchant_id)
		FROM billable_events
		WHERE created_at >= ? AND is_billable = true
	`, sevenDaysAgo).Scan(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

// RunAggregation performs the full aggregation pipeline for a single window.
// Exported (capital R) so integration tests can call it directly.
func RunAggregation(db *gorm.DB, snapshotDate time.Time, windowDays int) error {
	// ==========================================
	// 0. PRE-FLIGHT MERCHANT DENSITY CHECK
	// ==========================================
	minMerchantCount := 30
	if envVal := os.Getenv("SIGNALS_MIN_MERCHANT_COUNT"); envVal != "" {
		if val, err := strconv.Atoi(envVal); err == nil {
			minMerchantCount = val
		}
	}

	activeMerchants, err := countActiveMerchants(db)
	if err != nil {
		return fmt.Errorf("failed to check merchant density: %w", err)
	}

	if activeMerchants < minMerchantCount {
		slog.Info(fmt.Sprintf("Signals worker skipping: only %d active merchants found, minimum threshold is %d. Re-check after more merchants onboard.", activeMerchants, minMerchantCount))
		return nil // skip aggregation and exit cleanly
	}

	start := time.Now()
	slog.Info("running aggregation", "snapshot_date", snapshotDate.Format("2006-01-02"), "window_days", windowDays)

	rangeStart, rangeEnd := computeTimeRange(snapshotDate, windowDays)
	dateStr := snapshotDate.Format("2006-01-02")

	// ==========================================
	// 1. CATEGORY SIGNALS AGGREGATION
	// ==========================================
	var catResults []AggregationRow
	err = db.Raw(`
		SELECT category_l1, category_l2, geo_state, geo_tier,
		       COUNT(*) as order_count,
		       COUNT(DISTINCT merchant_id) as merchant_count,
		       SUM(order_value_paise) as total_gmv_paise,
		       AVG(CASE WHEN is_rto THEN 1.0 ELSE 0.0 END) as rto_rate,
		       AVG(CASE WHEN payment_method = 'cod' THEN 1.0 ELSE 0.0 END) as cod_share,
		       AVG(order_value_paise) as avg_order_value_paise
		FROM billable_events
		WHERE created_at >= ? AND created_at < ?
		  AND category_l1 != ''
		  AND geo_state != ''
		  AND is_billable = true
		GROUP BY category_l1, category_l2, geo_state, geo_tier
	`, rangeStart, rangeEnd).Scan(&catResults).Error
	if err != nil {
		return fmt.Errorf("category aggregation query failed: %w", err)
	}

	catPassed := 0
	catDropped := 0
	for _, row := range catResults {
		if row.MerchantCount < kAnonymityThreshold {
			catDropped++
			continue
		}
		catPassed++

		cohortKey := fmt.Sprintf("cat|%s|%s|%s|%d|%d", row.CategoryL1, row.CategoryL2, row.GeoState, row.GeoTier, windowDays)
		gmvIndexed, aovIndexed, err := resolveIndex(db, cohortKey, row.TotalGMVPaise, row.AvgOrderValuePaise, snapshotDate)
		if err != nil {
			return fmt.Errorf("index resolution failed for %s: %w", cohortKey, err)
		}

		if err := upsertCategorySignal(db, row, dateStr, windowDays, gmvIndexed, aovIndexed); err != nil {
			return fmt.Errorf("category_signals upsert failed: %w", err)
		}
	}

	// ==========================================
	// 2. GEO SIGNALS AGGREGATION
	// ==========================================
	var geoResults []GeoAggregationRow
	err = db.Raw(`
		SELECT geo_state, geo_tier,
		       COUNT(*) as order_count,
		       COUNT(DISTINCT merchant_id) as merchant_count,
		       SUM(order_value_paise) as total_gmv_paise,
		       AVG(CASE WHEN is_rto THEN 1.0 ELSE 0.0 END) as rto_rate,
		       AVG(CASE WHEN payment_method = 'cod' THEN 1.0 ELSE 0.0 END) as cod_share
		FROM billable_events
		WHERE created_at >= ? AND created_at < ?
		  AND geo_state != ''
		  AND is_billable = true
		GROUP BY geo_state, geo_tier
	`, rangeStart, rangeEnd).Scan(&geoResults).Error
	if err != nil {
		return fmt.Errorf("geo aggregation query failed: %w", err)
	}

	geoPassed := 0
	geoDropped := 0
	for _, row := range geoResults {
		if row.MerchantCount < kAnonymityThreshold {
			geoDropped++
			continue
		}
		geoPassed++

		cohortKey := fmt.Sprintf("geo|%s|%d|%d", row.GeoState, row.GeoTier, windowDays)
		gmvIndexed, _, err := resolveIndex(db, cohortKey, row.TotalGMVPaise, 0, snapshotDate)
		if err != nil {
			return fmt.Errorf("index resolution failed for %s: %w", cohortKey, err)
		}

		if err := upsertGeoSignal(db, row, dateStr, windowDays, gmvIndexed); err != nil {
			return fmt.Errorf("geo_signals upsert failed: %w", err)
		}
	}

	// ==========================================
	// 3. PAYMENT SIGNALS AGGREGATION
	// ==========================================
	var payResults []PaymentAggregationRow
	err = db.Raw(`
		SELECT category_l1, geo_state,
		       COUNT(DISTINCT merchant_id) as merchant_count,
		       AVG(CASE WHEN payment_method = 'cod' THEN 1.0 ELSE 0.0 END) as cod_share
		FROM billable_events
		WHERE created_at >= ? AND created_at < ?
		  AND category_l1 != ''
		  AND geo_state != ''
		  AND is_billable = true
		GROUP BY category_l1, geo_state
	`, rangeStart, rangeEnd).Scan(&payResults).Error
	if err != nil {
		return fmt.Errorf("payment aggregation query failed: %w", err)
	}

	payPassed := 0
	payDropped := 0
	for _, row := range payResults {
		if row.MerchantCount < kAnonymityThreshold {
			payDropped++
			continue
		}
		payPassed++

		prepaidShare := 1.0 - row.CODShare
		codShareChange2d := computeCODShareChange(db, row.CategoryL1, row.GeoState, dateStr, windowDays, row.CODShare)

		if err := upsertPaymentSignal(db, row, dateStr, windowDays, prepaidShare, codShareChange2d); err != nil {
			return fmt.Errorf("payment_signals upsert failed: %w", err)
		}
	}

	elapsed := time.Since(start)
	slog.Info("aggregation complete",
		"snapshot_date", dateStr,
		"window_days", windowDays,
		"category_cohorts_written", catPassed,
		"category_cohorts_dropped", catDropped,
		"geo_cohorts_written", geoPassed,
		"geo_cohorts_dropped", geoDropped,
		"payment_cohorts_written", payPassed,
		"payment_cohorts_dropped", payDropped,
		"runtime_ms", elapsed.Milliseconds(),
	)
	return nil
}

// computeTimeRange returns the [start, end) time range in IST for the given window.
// For window_days=1: yesterday 00:00:00 IST → today 00:00:00 IST.
// For window_days=N: N days ago 00:00:00 IST → today 00:00:00 IST.
func computeTimeRange(snapshotDate time.Time, windowDays int) (start, end time.Time) {
	end = snapshotDate
	start = snapshotDate.AddDate(0, 0, -windowDays)
	return start, end
}

// resolveIndex checks signals.index_base_periods for an existing base.
// If none exists, this run's values become the base (index = 100.00).
func resolveIndex(db *gorm.DB, cohortKey string, totalGMVPaise int64, avgOrderValuePaise float64, baseDate time.Time) (gmvIndexed, aovIndexed float64, err error) {
	var base IndexBasePeriod
	result := db.Raw(`SELECT cohort_key, base_gmv, base_aov FROM signals.index_base_periods WHERE cohort_key = ?`, cohortKey).Scan(&base)
	if result.Error != nil {
		return 0, 0, fmt.Errorf("index base lookup failed: %w", result.Error)
	}

	if result.RowsAffected == 0 {
		// First time for this cohort — this run becomes the base period.
		insertResult := db.Exec(`
			INSERT INTO signals.index_base_periods (cohort_key, base_gmv, base_aov, base_date)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (cohort_key) DO NOTHING
		`, cohortKey, totalGMVPaise, avgOrderValuePaise, baseDate.Format("2006-01-02"))
		if insertResult.Error != nil {
			return 0, 0, fmt.Errorf("index base insert failed: %w", insertResult.Error)
		}
		return 100.00, 100.00, nil
	}

	// Base exists — compute indexed values.
	if base.BaseGMV > 0 {
		gmvIndexed = (float64(totalGMVPaise) / float64(base.BaseGMV)) * 100.0
	}
	if base.BaseAOV > 0 {
		aovIndexed = (avgOrderValuePaise / base.BaseAOV) * 100.0
	}
	return gmvIndexed, aovIndexed, nil
}

// upsertCategorySignal writes a single cohort row into signals.category_signals.
func upsertCategorySignal(db *gorm.DB, row AggregationRow, dateStr string, windowDays int, gmvIndexed, aovIndexed float64) error {
	return db.Exec(`
		INSERT INTO signals.category_signals
			(category_l1, category_l2, geo_state, geo_tier, snapshot_date, window_days,
			 order_count, merchant_count, gmv_indexed, rto_rate, cod_share, aov_indexed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (category_l1, category_l2, geo_state, geo_tier, snapshot_date, window_days)
		DO UPDATE SET
			order_count = EXCLUDED.order_count,
			merchant_count = EXCLUDED.merchant_count,
			gmv_indexed = EXCLUDED.gmv_indexed,
			rto_rate = EXCLUDED.rto_rate,
			cod_share = EXCLUDED.cod_share,
			aov_indexed = EXCLUDED.aov_indexed
	`, row.CategoryL1, row.CategoryL2, row.GeoState, row.GeoTier, dateStr, windowDays,
		row.OrderCount, row.MerchantCount, gmvIndexed, row.RTORate, row.CODShare, aovIndexed,
	).Error
}

// upsertGeoSignal writes an aggregated geo-level row into signals.geo_signals.
func upsertGeoSignal(db *gorm.DB, row GeoAggregationRow, dateStr string, windowDays int, gmvIndexed float64) error {
	return db.Exec(`
		INSERT INTO signals.geo_signals
			(geo_state, geo_tier, snapshot_date, window_days,
			 order_count, merchant_count, gmv_indexed, rto_rate, cod_share)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (geo_state, geo_tier, snapshot_date, window_days)
		DO UPDATE SET
			order_count = EXCLUDED.order_count,
			merchant_count = EXCLUDED.merchant_count,
			gmv_indexed = EXCLUDED.gmv_indexed,
			rto_rate = EXCLUDED.rto_rate,
			cod_share = EXCLUDED.cod_share
	`, row.GeoState, row.GeoTier, dateStr, windowDays,
		row.OrderCount, row.MerchantCount, gmvIndexed, row.RTORate, row.CODShare,
	).Error
}

// upsertPaymentSignal writes a payment-method-level row into signals.payment_signals.
func upsertPaymentSignal(db *gorm.DB, row PaymentAggregationRow, dateStr string, windowDays int, prepaidShare float64, codShareChange2d *float64) error {
	return db.Exec(`
		INSERT INTO signals.payment_signals
			(category_l1, geo_state, snapshot_date, window_days,
			 cod_share, prepaid_share, cod_share_change_2d, merchant_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (category_l1, geo_state, snapshot_date, window_days)
		DO UPDATE SET
			cod_share = EXCLUDED.cod_share,
			prepaid_share = EXCLUDED.prepaid_share,
			cod_share_change_2d = EXCLUDED.cod_share_change_2d,
			merchant_count = EXCLUDED.merchant_count
	`, row.CategoryL1, row.GeoState, dateStr, windowDays,
		row.CODShare, prepaidShare, codShareChange2d, row.MerchantCount,
	).Error
}

// computeCODShareChange looks up the prior snapshot's cod_share and returns the delta.
// Returns nil if no prior data exists.
func computeCODShareChange(db *gorm.DB, catL1, geoState, currentDateStr string, windowDays int, currentCODShare float64) *float64 {
	var priorCODShare float64
	result := db.Raw(`
		SELECT cod_share FROM signals.payment_signals
		WHERE category_l1 = ? AND geo_state = ? AND window_days = ?
		  AND snapshot_date < ?
		ORDER BY snapshot_date DESC
		LIMIT 1
	`, catL1, geoState, windowDays, currentDateStr).Scan(&priorCODShare)

	if result.Error != nil || result.RowsAffected == 0 {
		return nil
	}
	change := currentCODShare - priorCODShare
	return &change
}
