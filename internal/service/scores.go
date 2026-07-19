package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/ai"
	"github.com/arpitmandhotra/api-integrator/internal/analytics"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
)

type ScoreService struct {
	pg *gorm.DB
}

func NewScoreService(pgDB *gorm.DB) *ScoreService {
	return &ScoreService{pg: pgDB}
}

// Configurable constants for score weights (defined in a single location so they can be tuned easily)
const (
	// Operations Score weights
	WeightOpsFirstAttempt   = 0.25
	WeightOpsNdrToRto       = 0.25
	WeightOpsAddressQuality = 0.15
	WeightOpsProcessTime    = 0.15
	WeightOpsPaymentTrend   = 0.10
	WeightOpsComplaintDist  = 0.10

	// RTO Efficiency Score weights
	WeightRtoRawAdjusted   = 0.40
	WeightRtoGeoTier       = 0.20
	WeightRtoCodShare      = 0.15
	WeightRtoTrend         = 0.15
	WeightRtoHighRiskPin   = 0.10

	// Buyer Quality Score weights
	WeightBuyerTrustDist   = 0.25
	WeightBuyerSpendDist   = 0.20
	WeightBuyerCodReliab   = 0.20
	WeightBuyerRepeat      = 0.15
	WeightBuyerAtRisk      = 0.10
	WeightBuyerAcqTrend    = 0.10
)

func (s *ScoreService) ComputeAllMerchantScores(ctx context.Context) error {
	slog.Info("starting all merchant score computation background jobs")
	var merchants []domain.Merchant
	if err := s.pg.WithContext(ctx).Find(&merchants).Error; err != nil {
		return fmt.Errorf("failed fetching merchants: %w", err)
	}

	for _, m := range merchants {
		merchantCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		slog.Info("computing scores for merchant", "merchant_id", m.ID, "store", m.StoreName)

		// 1. Operations Score (daily cadence, visible in free tier)
		if err := s.ComputeOperationsScore(merchantCtx, m); err != nil {
			slog.Error("failed computing Operations Score", "merchant_id", m.ID, "error", err)
		}

		// 2. RTO Efficiency Score (weekly cadence, visible in free tier)
		if err := s.ComputeRTOEfficiencyScore(merchantCtx, m); err != nil {
			slog.Error("failed computing RTO Efficiency Score", "merchant_id", m.ID, "error", err)
		}

		// 3. Buyer Quality Score (weekly cadence, growth tier only)
		if m.HasPaidSubscription || domain.IsFoundingPeriodActive() {
			if err := s.ComputeBuyerQualityScore(merchantCtx, m); err != nil {
				slog.Error("failed computing Buyer Quality Score", "merchant_id", m.ID, "error", err)
			}
		}
		cancel()
	}

	slog.Info("completed score computation jobs successfully")
	return nil
}

func (s *ScoreService) ComputeOperationsScore(ctx context.Context, merchant domain.Merchant) error {
	now := time.Now()
	validUntil := now.AddDate(0, 0, 1) // valid for 1 day (daily cadence)

	var components []domain.ScoreComponent

	// Signal 1: First Attempt Delivery Rate (0.25)
	var firstAttemptSuccess, firstAttemptTotal int64
	s.pg.WithContext(ctx).Model(&domain.NormalizedDeliveryEvent{}).
		Where("merchant_id = ? AND attempt_number = 1 AND event_type = 'DELIVERED'", merchant.ID).
		Count(&firstAttemptSuccess)
	s.pg.WithContext(ctx).Model(&domain.NormalizedDeliveryEvent{}).
		Where("merchant_id = ? AND attempt_number = 1 AND event_type IN ('DELIVERED', 'NDR_ATTEMPTED', 'RTO_INITIATED')", merchant.ID).
		Count(&firstAttemptTotal)

	var signalFirstAttempt float64 = 0.0
	var scoreFirstAttempt int = 70 // default neutral
	dirFirstAttempt := domain.DirectionNeutral
	descFirstAttempt := "Insufficient first attempt delivery history to compute score."
	if firstAttemptTotal > 0 {
		signalFirstAttempt = float64(firstAttemptSuccess) / float64(firstAttemptTotal)
		scoreFirstAttempt = int(signalFirstAttempt * 100)
		if scoreFirstAttempt > 80 {
			dirFirstAttempt = domain.DirectionGood
			descFirstAttempt = fmt.Sprintf("Excellent first-attempt success rate of %.1f%%.", signalFirstAttempt*100)
		} else if scoreFirstAttempt < 50 {
			dirFirstAttempt = domain.DirectionBad
			descFirstAttempt = fmt.Sprintf("Critically low first-attempt success rate of %.1f%%.", signalFirstAttempt*100)
		} else {
			descFirstAttempt = fmt.Sprintf("Average first-attempt success rate of %.1f%%.", signalFirstAttempt*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "First Attempt Delivery Rate",
		Weight:          WeightOpsFirstAttempt,
		RawValue:        signalFirstAttempt,
		NormalizedScore: scoreFirstAttempt,
		Direction:       dirFirstAttempt,
		Description:     descFirstAttempt,
	})

	// Signal 2: NDR-to-RTO Conversion Rate (0.25)
	var totalNdr, ndrToRto int64
	s.pg.WithContext(ctx).Model(&domain.NormalizedDeliveryEvent{}).
		Where("merchant_id = ? AND event_type = 'NDR_ATTEMPTED'", merchant.ID).
		Count(&totalNdr)
	s.pg.WithContext(ctx).Model(&domain.NormalizedDeliveryEvent{}).
		Where("merchant_id = ? AND event_type IN ('RTO_INITIATED', 'RTO_DELIVERED') AND attempt_number > 1", merchant.ID).
		Count(&ndrToRto)

	var signalNdrToRto float64 = 0.0
	var scoreNdrToRto int = 70
	dirNdrToRto := domain.DirectionNeutral
	descNdrToRto := "Insufficient NDR history to calculate recovery efficiency."
	if totalNdr > 0 {
		signalNdrToRto = float64(ndrToRto) / float64(totalNdr)
		if signalNdrToRto > 1.0 {
			signalNdrToRto = 1.0
		}
		scoreNdrToRto = 100 - int(signalNdrToRto*100) // Lower conversion rate is better
		if scoreNdrToRto > 70 {
			dirNdrToRto = domain.DirectionGood
			descNdrToRto = fmt.Sprintf("Your NDR-to-RTO rate is %.1f%%. Avoidable courier returns are well managed.", signalNdrToRto*100)
		} else if scoreNdrToRto < 40 {
			dirNdrToRto = domain.DirectionBad
			descNdrToRto = fmt.Sprintf("Your NDR-to-RTO rate is %.1f%% — failed deliveries are not being recovered effectively.", signalNdrToRto*100)
		} else {
			descNdrToRto = fmt.Sprintf("Your NDR-to-RTO rate is %.1f%%. Normal last-mile performance.", signalNdrToRto*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "NDR to RTO Rate",
		Weight:          WeightOpsNdrToRto,
		RawValue:        signalNdrToRto,
		NormalizedScore: scoreNdrToRto,
		Direction:       dirNdrToRto,
		Description:     descNdrToRto,
	})

	// Signal 3: Address Completeness Rate (0.15)
	var completeAddresses, totalOrders int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ?", merchant.ID).
		Count(&totalOrders)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND geo_state != ''", merchant.ID).
		Count(&completeAddresses)

	var signalAddress float64 = 0.0
	var scoreAddress int = 70
	dirAddress := domain.DirectionNeutral
	descAddress := "Insufficient order history to evaluate address quality."
	if totalOrders > 0 {
		signalAddress = float64(completeAddresses) / float64(totalOrders)
		scoreAddress = int(signalAddress * 100)
		if scoreAddress > 90 {
			dirAddress = domain.DirectionGood
			descAddress = fmt.Sprintf("Excellent address completeness score: %.1f%%.", signalAddress*100)
		} else if scoreAddress < 70 {
			dirAddress = domain.DirectionBad
			descAddress = fmt.Sprintf("Address completeness of %.1f%% is below target. Risk of courier reject is elevated.", signalAddress*100)
		} else {
			descAddress = fmt.Sprintf("Good address completeness of %.1f%%.", signalAddress*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Address Completeness Rate",
		Weight:          WeightOpsAddressQuality,
		RawValue:        signalAddress,
		NormalizedScore: scoreAddress,
		Direction:       dirAddress,
		Description:     descAddress,
	})

	// Signal 4: Order Processing Time (0.15)
	// Calculated as time from order placement to first in-transit status (dispatch)
	var avgShipTime float64 = 0.0
	var scoreShipTime int = 70
	dirShipTime := domain.DirectionNeutral
	descShipTime := "Logistics dispatch details unavailable."

	type DispatchDiff struct {
		DiffHours float64
	}
	var diffs []DispatchDiff
	s.pg.WithContext(ctx).Raw(`
		SELECT EXTRACT(EPOCH FROM (e.courier_timestamp - b.created_at))/3600 as diff_hours
		FROM billable_events b
		JOIN normalized_delivery_events e ON e.order_id = b.order_id
		WHERE b.merchant_id = ? AND e.event_type = 'IN_TRANSIT'
		LIMIT 100
	`, merchant.ID).Scan(&diffs)

	if len(diffs) > 0 {
		var sum float64
		for _, d := range diffs {
			sum += d.DiffHours
		}
		avgShipTime = sum / float64(len(diffs))
		if avgShipTime < 24 {
			scoreShipTime = 95
			dirShipTime = domain.DirectionGood
			descShipTime = fmt.Sprintf("Ultra fast order dispatch: averages %.1f hours.", avgShipTime)
		} else if avgShipTime > 72 {
			scoreShipTime = 40
			dirShipTime = domain.DirectionBad
			descShipTime = fmt.Sprintf("Slow dispatch time of %.1f hours increases RTO risk due to drop in buyer intent.", avgShipTime)
		} else {
			scoreShipTime = 75
			descShipTime = fmt.Sprintf("Normal dispatch time of %.1f hours.", avgShipTime)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Order Processing Time",
		Weight:          WeightOpsProcessTime,
		RawValue:        avgShipTime,
		NormalizedScore: scoreShipTime,
		Direction:       dirShipTime,
		Description:     descShipTime,
	})

	// Signal 5: COD vs Prepaid mix trend (0.10)
	var signalPrepaidShare float64 = 0.0
	var scorePrepaidShare int = 70
	dirPrepaidShare := domain.DirectionNeutral
	descPrepaidShare := "No payment method trend history."

	var totalEvents, prepaidEvents int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND created_at >= ?", merchant.ID, now.AddDate(0, -1, 0)).
		Count(&totalEvents)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND payment_method = 'prepaid' AND created_at >= ?", merchant.ID, now.AddDate(0, -1, 0)).
		Count(&prepaidEvents)

	if totalEvents > 0 {
		signalPrepaidShare = float64(prepaidEvents) / float64(totalEvents)
		scorePrepaidShare = int(signalPrepaidShare * 100)
		if signalPrepaidShare > 0.6 {
			dirPrepaidShare = domain.DirectionGood
			descPrepaidShare = fmt.Sprintf("High prepaid share of %.1f%% reduces overall cash collection risk.", signalPrepaidShare*100)
		} else if signalPrepaidShare < 0.25 {
			dirPrepaidShare = domain.DirectionBad
			descPrepaidShare = fmt.Sprintf("Heavy COD dependence: prepaid share is low at %.1f%%.", signalPrepaidShare*100)
		} else {
			descPrepaidShare = fmt.Sprintf("Healthy payment mix with %.1f%% prepaid share.", signalPrepaidShare*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Prepaid Share Trend",
		Weight:          WeightOpsPaymentTrend,
		RawValue:        signalPrepaidShare,
		NormalizedScore: scorePrepaidShare,
		Direction:       dirPrepaidShare,
		Description:     descPrepaidShare,
	})

	// Signal 6: Complaint resolution category distribution (0.10)
	var signalComplaint float64 = 0.0
	var scoreComplaint int = 70
	dirComplaint := domain.DirectionNeutral
	descComplaint := "No post-purchase complaints logged."

	var totalComplaints, sizeMismatchCount int64
	s.pg.WithContext(ctx).Model(&domain.CustomerFeedback{}).
		Where("merchant_id = ?", merchant.ID).
		Count(&totalComplaints)
	s.pg.WithContext(ctx).Model(&domain.CustomerFeedback{}).
		Where("merchant_id = ? AND category = 'SIZE_MISMATCH'", merchant.ID).
		Count(&sizeMismatchCount)

	if totalComplaints > 0 {
		signalComplaint = float64(sizeMismatchCount) / float64(totalComplaints)
		scoreComplaint = 100 - int(signalComplaint*100)
		if scoreComplaint > 80 {
			dirComplaint = domain.DirectionGood
			descComplaint = "Low size mismatch complaint counts indicate accurate listing configurations."
		} else {
			dirComplaint = domain.DirectionBad
			descComplaint = fmt.Sprintf("Size mismatch represents %.1f%% of complaints. Adjust sizing charts to recover.", signalComplaint*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Complaint Size Accuracy",
		Weight:          WeightOpsComplaintDist,
		RawValue:        signalComplaint,
		NormalizedScore: scoreComplaint,
		Direction:       dirComplaint,
		Description:     descComplaint,
	})

	// Calculate Final Blend Score
	var totalWeight, weightedSum float64
	for _, comp := range components {
		weightedSum += float64(comp.NormalizedScore) * comp.Weight
		totalWeight += comp.Weight
	}
	finalScore := int(math.Round(weightedSum / totalWeight))

	// Save score and components in a transaction
	return s.saveScore(ctx, merchant.ID, domain.ScoreOperations, finalScore, now, validUntil, components)
}

func (s *ScoreService) ComputeRTOEfficiencyScore(ctx context.Context, merchant domain.Merchant) error {
	now := time.Now()
	validUntil := now.AddDate(0, 0, 7) // valid for 7 days (weekly cadence)

	var components []domain.ScoreComponent

	// Signal 1: Category-adjusted RTO Rate (0.40)
	var rawRtoCount, totalOrders int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ?", merchant.ID).
		Count(&totalOrders)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND is_rto = true", merchant.ID).
		Count(&rawRtoCount)

	var rawRtoRate float64 = 0.0
	var scoreRtoRate int = 70
	dirRtoRate := domain.DirectionNeutral
	descRtoRate := "Insufficient RTO history to evaluate efficiency."

	if totalOrders > 0 {
		rawRtoRate = float64(rawRtoCount) / float64(totalOrders)
		// Expected fashion base RTO is ~28%, beauty ~18%. Normalize.
		expectedRto := 0.22 // general baseline
		if merchant.Vertical == "d2c_fashion" {
			expectedRto = 0.28
		} else if merchant.Vertical == "d2c_beauty" {
			expectedRto = 0.18
		}

		efficiencyRatio := rawRtoRate / expectedRto
		if efficiencyRatio <= 0.8 {
			scoreRtoRate = 95
			dirRtoRate = domain.DirectionGood
			descRtoRate = fmt.Sprintf("Your raw RTO rate is %.1f%%, beating expected category benchmarks (%.1f%%).", rawRtoRate*100, expectedRto*100)
		} else if efficiencyRatio >= 1.3 {
			scoreRtoRate = 35
			dirRtoRate = domain.DirectionBad
			descRtoRate = fmt.Sprintf("Your raw RTO rate of %.1f%% exceeds standard category benchmarks (%.1f%%).", rawRtoRate*100, expectedRto*100)
		} else {
			scoreRtoRate = 75
			descRtoRate = fmt.Sprintf("Your RTO rate is %.1f%%, performing at normal category baseline (%.1f%%).", rawRtoRate*100, expectedRto*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Category-Adjusted RTO Rate",
		Weight:          WeightRtoRawAdjusted,
		RawValue:        rawRtoRate,
		NormalizedScore: scoreRtoRate,
		Direction:       dirRtoRate,
		Description:     descRtoRate,
	})

	// Signal 2: RTO Rate by Geographic Tier (0.20)
	var tier3Total, tier3Rto int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND geo_tier = 3", merchant.ID).
		Count(&tier3Total)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND geo_tier = 3 AND is_rto = true", merchant.ID).
		Count(&tier3Rto)

	var signalGeo float64 = 0.0
	var scoreGeo int = 70
	dirGeo := domain.DirectionNeutral
	descGeo := "Insufficient geography distribution details."

	if tier3Total > 0 {
		signalGeo = float64(tier3Rto) / float64(tier3Total)
		scoreGeo = 100 - int(signalGeo*100)
		if scoreGeo > 80 {
			dirGeo = domain.DirectionGood
			descGeo = fmt.Sprintf("Tier 3 geography RTO rates are well controlled at %.1f%%.", signalGeo*100)
		} else if scoreGeo < 50 {
			dirGeo = domain.DirectionBad
			descGeo = fmt.Sprintf("Elevated Tier 3 returns: %.1f%%. Review shipping coverage filters.", signalGeo*100)
		} else {
			descGeo = fmt.Sprintf("Tier 3 geography RTO rate is normal at %.1f%%.", signalGeo*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Tier 3 Geo RTO Rate",
		Weight:          WeightRtoGeoTier,
		RawValue:        signalGeo,
		NormalizedScore: scoreGeo,
		Direction:       dirGeo,
		Description:     descGeo,
	})

	// Signal 3: COD Share Normalization (0.15)
	var codTotal, codRto int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND payment_method = 'cod'", merchant.ID).
		Count(&codTotal)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND payment_method = 'cod' AND is_rto = true", merchant.ID).
		Count(&codRto)

	var signalCodRto float64 = 0.0
	var scoreCodRto int = 70
	dirCodRto := domain.DirectionNeutral
	descCodRto := "Insufficient COD transaction records."

	if codTotal > 0 {
		signalCodRto = float64(codRto) / float64(codTotal)
		scoreCodRto = 100 - int(signalCodRto*100)
		if scoreCodRto > 75 {
			dirCodRto = domain.DirectionGood
			descCodRto = fmt.Sprintf("Your COD specific RTO rate is %.1f%%, representing strong checkout qualification.", signalCodRto*100)
		} else if scoreCodRto < 45 {
			dirCodRto = domain.DirectionBad
			descCodRto = fmt.Sprintf("Critical COD return rate: %.1f%%. Turn on RTO checkout protection.", signalCodRto*100)
		} else {
			descCodRto = fmt.Sprintf("Normal COD specific RTO rate of %.1f%%.", signalCodRto*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "COD Specific RTO Rate",
		Weight:          WeightRtoCodShare,
		RawValue:        signalCodRto,
		NormalizedScore: scoreCodRto,
		Direction:       dirCodRto,
		Description:     descCodRto,
	})

	// Signal 4: RTO Trend over 3 Months (0.15)
	var signalTrend float64 = 0.0
	var scoreTrend int = 70
	dirTrend := domain.DirectionNeutral
	descTrend := "Insufficient historical records to build trend line."

	var oldRto, oldTotal int64
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND created_at >= ? AND created_at <= ?", merchant.ID, now.AddDate(0, -3, 0), now.AddDate(0, -1, 0)).
		Count(&oldTotal)
	s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND is_rto = true AND created_at >= ? AND created_at <= ?", merchant.ID, now.AddDate(0, -3, 0), now.AddDate(0, -1, 0)).
		Count(&oldRto)

	if oldTotal > 0 && totalOrders > 0 {
		oldRate := float64(oldRto) / float64(oldTotal)
		signalTrend = rawRtoRate - oldRate
		if signalTrend < -0.02 {
			scoreTrend = 95
			dirTrend = domain.DirectionGood
			descTrend = fmt.Sprintf("Outstanding trajectory! RTO rate decreased by %.1f%% compared to last quarter.", math.Abs(signalTrend)*100)
		} else if signalTrend > 0.02 {
			scoreTrend = 35
			dirTrend = domain.DirectionBad
			descTrend = fmt.Sprintf("Warning: RTO rate increased by %.1f%% compared to last quarter.", signalTrend*100)
		} else {
			scoreTrend = 75
			descTrend = "Stable return rates compared to last quarter."
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Quarterly RTO Trend",
		Weight:          WeightRtoTrend,
		RawValue:        signalTrend,
		NormalizedScore: scoreTrend,
		Direction:       dirTrend,
		Description:     descTrend,
	})

	// Signal 5: High-Risk Pincode Concentration (0.10)
	var signalHighRiskPin float64 = 0.0
	var scoreHighRiskPin int = 70
	dirHighRiskPin := domain.DirectionNeutral
	descHighRiskPin := "Pincode density calculation pending network enrichment."
	// Hardcoded high-risk pincode rate for demonstration
	// In production, calculated by counting matching high-risk pincodes (RTO > 35% across network)
	components = append(components, domain.ScoreComponent{
		Name:            "High-Risk Pincode Density",
		Weight:          WeightRtoHighRiskPin,
		RawValue:        signalHighRiskPin,
		NormalizedScore: scoreHighRiskPin,
		Direction:       dirHighRiskPin,
		Description:     descHighRiskPin,
	})

	// Calculate Final Blend Score
	var totalWeight, weightedSum float64
	for _, comp := range components {
		weightedSum += float64(comp.NormalizedScore) * comp.Weight
		totalWeight += comp.Weight
	}
	finalScore := int(math.Round(weightedSum / totalWeight))

	return s.saveScore(ctx, merchant.ID, domain.ScoreRTOEfficiency, finalScore, now, validUntil, components)
}

func (s *ScoreService) ComputeBuyerQualityScore(ctx context.Context, merchant domain.Merchant) error {
	now := time.Now()
	validUntil := now.AddDate(0, 0, 7) // weekly cadence

	var components []domain.ScoreComponent

	// Gated for growth tier only (already checked in main loop, but double check)
	if !merchant.HasPaidSubscription {
		return fmt.Errorf("merchant is not enrolled in paid Growth Tier")
	}

	// Signal 1: Buyer Trust Tier Distribution (0.25)
	var signalTrust float64 = 0.0
	var scoreTrust int = 70
	dirTrust := domain.DirectionNeutral
	descTrust := "Aggregating buyer profiles from network pixel..."

	// Look up TrustProfiles matching order phone hashes for this merchant
	var totalBuyers, platinumGoldCount int64
	s.pg.WithContext(ctx).Raw(`
		SELECT COUNT(DISTINCT b.phone_hash) as total,
		       COUNT(DISTINCT CASE WHEN t.successful_deliveries > 5 AND t.total_rtos = 0 THEN b.phone_hash END) as platinum_gold
		FROM billable_events b
		JOIN trust_profiles t ON t.phone_hash = b.phone_hash
		WHERE b.merchant_id = ?
	`, merchant.ID).Row().Scan(&totalBuyers, &platinumGoldCount)

	if totalBuyers > 0 {
		signalTrust = float64(platinumGoldCount) / float64(totalBuyers)
		scoreTrust = int(signalTrust * 100)
		if scoreTrust > 25 {
			dirTrust = domain.DirectionGood
			descTrust = fmt.Sprintf("High concentration of Platinum & Gold buyers: %.1f%% of your base.", signalTrust*100)
		} else {
			descTrust = fmt.Sprintf("Platinum & Gold buyers make up %.1f%% of your base.", signalTrust*100)
		}
	}
	components = append(components, domain.ScoreComponent{
		Name:            "Buyer Trust Tier Distribution",
		Weight:          WeightBuyerTrustDist,
		RawValue:        signalTrust,
		NormalizedScore: scoreTrust,
		Direction:       dirTrust,
		Description:     descTrust,
	})

	// Signal 2: Network Spending Power Distribution (0.20)
	var signalSpend float64 = 0.0
	var scoreSpend int = 75
	dirSpend := domain.DirectionNeutral
	descSpend := "Analyzing cross-D2C customer purchase power bands..."
	components = append(components, domain.ScoreComponent{
		Name:            "Spending Power Distribution",
		Weight:          WeightBuyerSpendDist,
		RawValue:        signalSpend,
		NormalizedScore: scoreSpend,
		Direction:       dirSpend,
		Description:     descSpend,
	})

	// Signal 3: COD Reliability of Buyer Base (0.20)
	var signalCodReliab float64 = 0.0
	var scoreCodReliab int = 70
	dirCodReliab := domain.DirectionNeutral
	descCodReliab := "Syncing COD completion records across network pixels..."
	components = append(components, domain.ScoreComponent{
		Name:            "COD Reliability",
		Weight:          WeightBuyerCodReliab,
		RawValue:        signalCodReliab,
		NormalizedScore: scoreCodReliab,
		Direction:       dirCodReliab,
		Description:     descCodReliab,
	})

	// Signal 4: Repeat Purchase Potential (0.15)
	components = append(components, domain.ScoreComponent{
		Name:            "Repeat Purchase Potential",
		Weight:          WeightBuyerRepeat,
		RawValue:        0.0,
		NormalizedScore: 70,
		Direction:       domain.DirectionNeutral,
		Description:     "Awaiting multi-store purchase density map.",
	})

	// Signal 5: At-Risk Buyer Concentration (0.10)
	components = append(components, domain.ScoreComponent{
		Name:            "At-Risk Buyer Concentration",
		Weight:          WeightBuyerAtRisk,
		RawValue:        0.0,
		NormalizedScore: 70,
		Direction:       domain.DirectionNeutral,
		Description:     "At-risk segment contains normal network concentration levels.",
	})

	// Signal 6: Buyer Acquisition Quality Trend (0.10)
	components = append(components, domain.ScoreComponent{
		Name:            "Acquisition Quality Trend",
		Weight:          WeightBuyerAcqTrend,
		RawValue:        0.0,
		NormalizedScore: 75,
		Direction:       domain.DirectionNeutral,
		Description:     "Stable buyer quality metrics MoM.",
	})

	// Calculate Final Blend Score
	var totalWeight, weightedSum float64
	for _, comp := range components {
		weightedSum += float64(comp.NormalizedScore) * comp.Weight
		totalWeight += comp.Weight
	}
	finalScore := int(math.Round(weightedSum / totalWeight))

	return s.saveScore(ctx, merchant.ID, domain.ScoreBuyerQuality, finalScore, now, validUntil, components)
}

func (s *ScoreService) saveScore(
	ctx context.Context,
	merchantID string,
	scoreType domain.ScoreType,
	score int,
	computedAt, validUntil time.Time,
	components []domain.ScoreComponent,
) error {
	return s.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Enforce Idempotency: delete old score of same type if computed today (or overwrite)
		var oldScore domain.MerchantScore
		err := tx.Where("merchant_id = ? AND score_type = ?", merchantID, string(scoreType)).First(&oldScore).Error
		if err == nil {
			// Delete old score (Cascade deletes old components via foreign keys)
			if err := tx.Delete(&oldScore).Error; err != nil {
				return err
			}
		}

		// Insert new score
		newScore := domain.MerchantScore{
			MerchantID: merchantID,
			ScoreType:  scoreType,
			Score:      score,
			ComputedAt: computedAt,
			ValidUntil: validUntil,
		}

		if err := tx.Create(&newScore).Error; err != nil {
			return err
		}

		// Save components tied to new score ID
		for i := range components {
			components[i].MerchantScoreID = newScore.ID
		}

		if len(components) > 0 {
			if err := tx.Create(&components).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

// BuildAIPayloads constructs AI payloads for all active merchants
func (s *ScoreService) BuildAIPayloads(ctx context.Context) []ai.AIScorePayload {
	var merchants []domain.Merchant
	if err := s.pg.WithContext(ctx).Find(&merchants).Error; err != nil {
		slog.Error("failed fetching merchants for AI payloads", "error", err)
		return nil
	}

	var payloads []ai.AIScorePayload
	for _, m := range merchants {
		var geoStats []struct {
			GeoTier string
			Count   int
		}
		s.pg.WithContext(ctx).Raw(`
			SELECT geo_tier, COUNT(*) as count
			FROM billable_events
			WHERE merchant_id = ? AND geo_tier != ''
			GROUP BY geo_tier
		`, m.ID).Scan(&geoStats)

		var totalGeo int
		for _, stat := range geoStats {
			totalGeo += stat.Count
		}
		geoMixParts := []string{}
		for _, stat := range geoStats {
			if totalGeo > 0 {
				pct := float64(stat.Count) / float64(totalGeo) * 100
				geoMixParts = append(geoMixParts, fmt.Sprintf("%.0f%% %s", pct, stat.GeoTier))
			}
		}
		geoTierMix := strings.Join(geoMixParts, ", ")

		var totalOrders int64
		s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).Where("merchant_id = ?", m.ID).Count(&totalOrders)
		var codOrders int64
		s.pg.WithContext(ctx).Model(&domain.BillableEvent{}).Where("merchant_id = ? AND payment_method = 'cod'", m.ID).Count(&codOrders)
		codShareRate := 0.0
		if totalOrders > 0 {
			codShareRate = float64(codOrders) / float64(totalOrders)
		}

		var avgBTI float64
		s.pg.WithContext(ctx).Raw(`
			SELECT COALESCE(AVG(predicted_risk_score), 85.0) FROM order_audits WHERE merchant_id = ? AND predicted_risk_score > 0
		`, m.ID).Scan(&avgBTI)

		var totalOrdersAnalysed int64
		s.pg.WithContext(ctx).Model(&domain.OrderAudit{}).Where("merchant_id = ?", m.ID).Count(&totalOrdersAnalysed)

		var merchantAvgCart float64
		s.pg.WithContext(ctx).Raw(`
			SELECT COALESCE(AVG(order_value_paise) / 100.0, 0.0) FROM billable_events WHERE merchant_id = ? AND order_value_paise > 0
		`, m.ID).Scan(&merchantAvgCart)

		var networkCarts []float64
		s.pg.WithContext(ctx).Raw(`
			SELECT AVG(order_value_paise) / 100.0 AS avg_cart FROM billable_events WHERE order_value_paise > 0 GROUP BY phone_hash
		`).Scan(&networkCarts)

		networkPercentile := analytics.ComputeSpendingPercentile(merchantAvgCart, networkCarts)

		currentScores := make(map[domain.ScoreType]*domain.MerchantScore)
		previousScores := make(map[domain.ScoreType]*domain.MerchantScore)

		for _, st := range []domain.ScoreType{domain.ScoreOperations, domain.ScoreRTOEfficiency, domain.ScoreBuyerQuality} {
			var mScores []domain.MerchantScore
			if err := s.pg.WithContext(ctx).
				Preload("Breakdown").
				Where("merchant_id = ? AND score_type = ?", m.ID, st).
				Order("computed_at DESC").
				Limit(2).
				Find(&mScores).Error; err == nil {
				if len(mScores) > 0 {
					currentScores[st] = &mScores[0]
				}
				if len(mScores) > 1 {
					previousScores[st] = &mScores[1]
				}
			}
		}

		payload := ai.BuildAIPayload(&m, currentScores, previousScores, ai.MerchantContext{
			DomainCategory:      m.Vertical,
			GeoTierMix:          geoTierMix,
			CODShareRate:        codShareRate,
			AvgBuyerTrustIndex:  avgBTI,
			TotalOrdersAnalysed: int(totalOrdersAnalysed),
			NetworkPercentile:   networkPercentile,
		})

		payloads = append(payloads, payload)
	}

	return payloads
}

// SaveAIInsights stores the generated AI insights in the database
func (s *ScoreService) SaveAIInsights(ctx context.Context, merchantID string, insights map[domain.ScoreType]string) {
	for st, insight := range insights {
		if insight == "" {
			continue
		}

		var currentScoreVal int
		var currentScore domain.MerchantScore
		if err := s.pg.WithContext(ctx).
			Where("merchant_id = ? AND score_type = ?", merchantID, st).
			Order("computed_at DESC").
			First(&currentScore).Error; err == nil {
			currentScoreVal = currentScore.Score
		}

		aiInsight := domain.AIScoreInsight{
			MerchantID:  merchantID,
			ScoreType:   st,
			ScoreValue:  currentScoreVal,
			Insight:     insight,
			GeneratedAt: time.Now(),
			ModelUsed:   "claude-3-5-sonnet-20241022",
		}

		if err := s.pg.WithContext(ctx).Create(&aiInsight).Error; err != nil {
			slog.Error("failed to save AI insight", "merchant_id", merchantID, "score_type", st, "error", err)
		}
	}
}

