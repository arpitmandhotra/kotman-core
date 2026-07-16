package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting scheduled buyer loyalty snapshot computation job")

	pg := database.NewPostgresClient()

	if err := RunBuyerLoyaltyJob(pg); err != nil {
		slog.Error("buyer loyalty job failed with error", "error", err)
		os.Exit(1)
	}

	slog.Info("buyer loyalty job completed successfully")
}

func RunBuyerLoyaltyJob(db *gorm.DB) error {
	ctx := context.Background()

	// 1. Fetch active merchants in the last 90 days
	var activeMerchantIDs []string
	ninetyDaysAgo := time.Now().AddDate(0, 0, -90)
	if err := db.Model(&domain.Order{}).
		Where("created_at >= ?", ninetyDaysAgo).
		Distinct("merchant_id").
		Pluck("merchant_id", &activeMerchantIDs).Error; err != nil {
		return fmt.Errorf("failed to fetch active merchants: %w", err)
	}

	slog.Info("identified active merchants to process", "count", len(activeMerchantIDs))

	// 2. Process merchants sequentially in batches of 50 with 100ms sleep
	batchSize := 50
	for i := 0; i < len(activeMerchantIDs); i += batchSize {
		end := i + batchSize
		if end > len(activeMerchantIDs) {
			end = len(activeMerchantIDs)
		}
		batch := activeMerchantIDs[i:end]

		slog.Info("processing merchant batch", "start_idx", i, "end_idx", end)

		for _, mIDStr := range batch {
			mID, err := uuid.Parse(mIDStr)
			if err != nil {
				slog.Error("invalid merchant UUID in orders", "merchant_id_str", mIDStr, "error", err)
				continue
			}

			// Run metrics computation inside a function for clean recovery/logging
			startTime := time.Now()
			err = computeMerchantLoyalty(ctx, db, mID)
			duration := time.Since(startTime)

			if err != nil {
				slog.Error("failed to compute buyer loyalty for merchant", "merchant_id", mID, "duration_ms", duration.Milliseconds(), "error", err)
			} else {
				slog.Info("successfully computed buyer loyalty for merchant", "merchant_id", mID, "duration_ms", duration.Milliseconds())
			}
		}

		// Sleep 100ms between batches to prevent database saturation
		if end < len(activeMerchantIDs) {
			time.Sleep(100 * time.Millisecond)
		}
	}

	return nil
}

type BuyerOrderCount struct {
	BuyerPhoneNormalized string `gorm:"column:buyer_phone_normalized"`
	OrderCount           int    `gorm:"column:order_count"`
}

func computeRepeatRate(ctx context.Context, db *gorm.DB, merchantID uuid.UUID, start, end time.Time) (int, int, float64, []BuyerOrderCount, error) {
	var counts []BuyerOrderCount
	err := db.WithContext(ctx).Model(&domain.Order{}).
		Select("buyer_phone_normalized, COUNT(id) as order_count").
		Where("merchant_id = ? AND buyer_phone_normalized IS NOT NULL AND buyer_phone_normalized != '' AND created_at >= ? AND created_at <= ?",
			merchantID, start, end).
		Group("buyer_phone_normalized").
		Scan(&counts).Error

	if err != nil {
		return 0, 0, 0, nil, err
	}

	totalUnique := len(counts)
	repeatBuyers := 0
	for _, c := range counts {
		if c.OrderCount >= 2 {
			repeatBuyers++
		}
	}

	rate := 0.0
	if totalUnique > 0 {
		rate = math.Round((float64(repeatBuyers)/float64(totalUnique))*1000) / 10
	}

	return totalUnique, repeatBuyers, rate, counts, nil
}

func computeMerchantLoyalty(ctx context.Context, db *gorm.DB, merchantID uuid.UUID) error {
	// Fetch the merchant to check tier
	var merchant domain.Merchant
	if err := db.WithContext(ctx).Where("id = ?", merchantID.String()).First(&merchant).Error; err != nil {
		return fmt.Errorf("merchant not found: %w", err)
	}

	now := time.Now().UTC()
	periodEnd := now
	periodStart := now.AddDate(0, 0, -30)
	prevEnd := periodStart
	prevStart := prevEnd.AddDate(0, 0, -30)

	// Step 1: Check history length (at least 30 days since the first order)
	var firstOrder domain.Order
	err := db.WithContext(ctx).
		Select("created_at").
		Where("merchant_id = ?", merchantID).
		Order("created_at ASC").
		First(&firstOrder).Error

	hasSufficientData := true
	insufficientReason := ""

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			hasSufficientData = false
			insufficientReason = "insufficient_history"
		} else {
			return fmt.Errorf("failed to check order history: %w", err)
		}
	} else if time.Since(firstOrder.CreatedAt) < 30*24*time.Hour {
		hasSufficientData = false
		insufficientReason = "insufficient_history"
	}

	// Step 2: Compute Metric 1 (Repeat Rate) for current and previous periods
	totalUnique, repeatCount, repeatRate, counts, err := computeRepeatRate(ctx, db, merchantID, periodStart, periodEnd)
	if err != nil {
		return fmt.Errorf("failed to compute current repeat rate: %w", err)
	}

	_, _, prevRepeatRate, _, err := computeRepeatRate(ctx, db, merchantID, prevStart, prevEnd)
	if err != nil {
		return fmt.Errorf("failed to compute previous repeat rate: %w", err)
	}

	// If unique buyers count is too low, override sufficiency check
	if totalUnique < 50 {
		hasSufficientData = false
		insufficientReason = "insufficient_buyers"
	}

	// Initialize snapshot values
	snapshot := domain.BuyerLoyaltySnapshot{
		ID:                     uuid.New(),
		MerchantID:             merchantID,
		ComputedAt:             now,
		PeriodStartAt:          periodStart,
		PeriodEndAt:            periodEnd,
		HasSufficientData:      hasSufficientData,
		InsufficientDataReason: insufficientReason,
	}

	// If sufficient data, set Metric 1 values
	if hasSufficientData {
		snapshot.TotalUniqueBuyers = totalUnique
		snapshot.RepeatBuyers = repeatCount
		snapshot.RepeatRatePct = repeatRate
		snapshot.PrevRepeatRatePct = prevRepeatRate
		snapshot.RepeatRateTrendPct = math.Round((repeatRate-prevRepeatRate)*10) / 10
	}

	// Step 3: Compute Metric 2 & 3 for Growth/GrowthAds tiers (or founding period)
	isGrowthOrAbove := domain.IsGrowthOrAbove(merchant.Tier) || domain.IsFoundingPeriodActive()

	if isGrowthOrAbove && hasSufficientData {
		// --- Metric 2 — True Repeat Rate ---
		var repeatPhones []string
		for _, c := range counts {
			if c.OrderCount >= 2 {
				repeatPhones = append(repeatPhones, c.BuyerPhoneNormalized)
			}
		}

		trueRepeatCount := 0
		if len(repeatPhones) > 0 {
			var rtoCounts []struct {
				PhoneNormalized string `gorm:"column:phone_normalized"`
				NetworkRTOCount int    `gorm:"column:network_rto_count"`
			}
			err = db.WithContext(ctx).Model(&domain.BuyerProfile{}).
				Select("phone_normalized, network_rto_count").
				Where("phone_normalized IN ?", repeatPhones).
				Scan(&rtoCounts).Error
			if err != nil {
				return fmt.Errorf("failed to query buyer profiles: %w", err)
			}

			rtoCountMap := make(map[string]int)
			for _, r := range rtoCounts {
				rtoCountMap[r.PhoneNormalized] = r.NetworkRTOCount
			}
			for _, phone := range repeatPhones {
				if rtoCountMap[phone] == 0 {
					trueRepeatCount++
				}
			}
		}

		// Previous period True Repeat Rate
		_, _, _, prevCounts, err := computeRepeatRate(ctx, db, merchantID, prevStart, prevEnd)
		if err != nil {
			return fmt.Errorf("failed to compute previous repeat rate: %w", err)
		}

		var prevRepeatPhones []string
		for _, c := range prevCounts {
			if c.OrderCount >= 2 {
				prevRepeatPhones = append(prevRepeatPhones, c.BuyerPhoneNormalized)
			}
		}

		prevTrueRepeatCount := 0
		if len(prevRepeatPhones) > 0 {
			var prevRTOCounts []struct {
				PhoneNormalized string `gorm:"column:phone_normalized"`
				NetworkRTOCount int    `gorm:"column:network_rto_count"`
			}
			err = db.WithContext(ctx).Model(&domain.BuyerProfile{}).
				Select("phone_normalized, network_rto_count").
				Where("phone_normalized IN ?", prevRepeatPhones).
				Scan(&prevRTOCounts).Error
			if err != nil {
				return fmt.Errorf("failed to query previous buyer profiles: %w", err)
			}

			prevRtoCountMap := make(map[string]int)
			for _, r := range prevRTOCounts {
				prevRtoCountMap[r.PhoneNormalized] = r.NetworkRTOCount
			}
			for _, phone := range prevRepeatPhones {
				if prevRtoCountMap[phone] == 0 {
					prevTrueRepeatCount++
				}
			}
		}

		trueRepeatRate := 0.0
		if totalUnique > 0 {
			trueRepeatRate = math.Round((float64(trueRepeatCount)/float64(totalUnique))*1000) / 10
		}

		prevTrueRepeatRate := 0.0
		var prevTotalUnique int64
		err = db.WithContext(ctx).Model(&domain.Order{}).
			Where("merchant_id = ? AND buyer_phone_normalized IS NOT NULL AND buyer_phone_normalized != '' AND created_at >= ? AND created_at <= ?",
				merchantID, prevStart, prevEnd).
			Distinct("buyer_phone_normalized").
			Count(&prevTotalUnique).Error
		if err == nil && prevTotalUnique > 0 {
			prevTrueRepeatRate = math.Round((float64(prevTrueRepeatCount)/float64(prevTotalUnique))*1000) / 10
		}

		snapshot.TrueRepeatBuyers = trueRepeatCount
		snapshot.TrueRepeatRatePct = trueRepeatRate
		snapshot.PrevTrueRepeatRatePct = prevTrueRepeatRate
		snapshot.TrueRepeatRateTrendPct = math.Round((trueRepeatRate-prevTrueRepeatRate)*10) / 10

		// --- Shopify Equivalent (Email-based) ---
		type EmailOrderCount struct {
			BuyerEmail string `gorm:"column:buyer_email"`
			OrderCount int    `gorm:"column:order_count"`
		}

		var emailCounts []EmailOrderCount
		err = db.WithContext(ctx).Model(&domain.Order{}).
			Select("buyer_email, COUNT(id) as order_count").
			Where("merchant_id = ? AND buyer_email IS NOT NULL AND buyer_email != '' AND created_at >= ? AND created_at <= ?",
				merchantID, periodStart, periodEnd).
			Group("buyer_email").
			Scan(&emailCounts).Error
		if err != nil {
			return fmt.Errorf("failed to query email-based counts: %w", err)
		}

		totalEmailUnique := len(emailCounts)
		repeatEmailBuyers := 0
		for _, ec := range emailCounts {
			if ec.OrderCount >= 2 {
				repeatEmailBuyers++
			}
		}

		if totalEmailUnique > 0 {
			shopifyVal := math.Round((float64(repeatEmailBuyers)/float64(totalEmailUnique))*1000) / 10
			snapshot.ShopifyEquivalentRepeatRatePct = &shopifyVal
		}

		// --- Metric 3 — Repeat RTO Abusers ---
		var merchantPhones []string
		for _, c := range counts {
			merchantPhones = append(merchantPhones, c.BuyerPhoneNormalized)
		}

		var abuserPhones []string
		if len(merchantPhones) > 0 {
			// Query abuser phones: network_total_orders >= 3 and network_rto_count / network_total_orders > 0.40
			err = db.WithContext(ctx).Model(&domain.BuyerProfile{}).
				Where("phone_normalized IN ? AND network_total_orders >= 3 AND network_total_orders > 0 AND (CAST(network_rto_count AS float) / CAST(network_total_orders AS float)) > 0.40", merchantPhones).
				Pluck("phone_normalized", &abuserPhones).Error
			if err != nil {
				return fmt.Errorf("failed to query repeat RTO abuser phones: %w", err)
			}
		}

		var totalRTOsOnMerchant int64 = 0
		if len(abuserPhones) > 0 {
			err = db.WithContext(ctx).Model(&domain.Order{}).
				Where("merchant_id = ? AND buyer_phone_normalized IN ? AND (outcome = 'RTO' OR outcome = 'RTO_DELIVERED') AND created_at >= ? AND created_at <= ?",
					merchantID, abuserPhones, periodStart, periodEnd).
				Count(&totalRTOsOnMerchant).Error
			if err != nil {
				return fmt.Errorf("failed to count abuser RTOs on merchant: %w", err)
			}
		}

		snapshot.RepeatRTOAbuserCount = len(abuserPhones)
		snapshot.RepeatRTOAbuserTotalRTOs = int(totalRTOsOnMerchant)
		snapshot.RepeatRTOAbuserEstimatedCostINR = int(totalRTOsOnMerchant) * domain.CostPerRTOINR
	}

	// 4. Upsert snapshot into database
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.BuyerLoyaltySnapshot
		dateStr := periodEnd.Format("2006-01-02")
		
		var dbErr error
		if db.Dialector.Name() == "sqlite" {
			dbErr = tx.Where("merchant_id = ? AND strftime('%Y-%m-%d', period_end_at) = ?", merchantID, dateStr).First(&existing).Error
		} else {
			dbErr = tx.Where("merchant_id = ? AND CAST(period_end_at AS date) = CAST(? AS date)", merchantID, periodEnd).First(&existing).Error
		}

		if dbErr == nil {
			snapshot.ID = existing.ID
			return tx.Save(&snapshot).Error
		} else if errors.Is(dbErr, gorm.ErrRecordNotFound) {
			return tx.Create(&snapshot).Error
		}
		return dbErr
	})

	if err != nil {
		return fmt.Errorf("failed to upsert buyer loyalty snapshot: %w", err)
	}

	return nil
}
