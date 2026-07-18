package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type AnalyticsHandler struct {
	pg    *gorm.DB
	redis *redis.Client
}

func NewAnalyticsHandler(pgDB *gorm.DB, redisClient *redis.Client) *AnalyticsHandler {
	return &AnalyticsHandler{
		pg:    pgDB,
		redis: redisClient,
	}
}

// GetMerchantInsights returns the full analytics payload for the merchant dashboard.
// Route: GET /v1/merchants/insights
// Auth: RequireAPIKey middleware (merchant context in c.Locals)
func (h *AnalyticsHandler) GetMerchantInsights(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	ctx := c.UserContext()

	var merchant domain.Merchant
	if err := h.pg.WithContext(ctx).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"success": false, "error": "merchant not found"})
	}

	// =====================================================================
	// META FIELDS
	// =====================================================================
	now := time.Now()
	executionMode := "ACTIVE" // shadow mode is removed altogether, always ACTIVE
	
	trialEndsAt := merchant.CreatedAt.AddDate(0, 0, 30)
	shadowDaysRemaining := 0
	if now.Before(trialEndsAt) {
		shadowDaysRemaining = int(time.Until(trialEndsAt).Hours() / 24)
	}

	var totalOrdersAnalyzed int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ?", merchantID).Count(&totalOrdersAnalyzed)

	minCohortMet := totalOrdersAnalyzed >= 50

	// =====================================================================
	// UPGRADE PROMPT LOGIC
	// Prompt daily if past the original 30 days trial and paid sub is false
	// =====================================================================
	showUpgradePrompt := false
	urgencyLevel := 0

	if !merchant.HasPaidSubscription {
		if now.After(trialEndsAt) {
			showUpgradePrompt = true
			urgencyLevel = 3 // urgent upgrade prompt every day
		} else if shadowDaysRemaining <= 5 {
			showUpgradePrompt = true
			urgencyLevel = 2
		} else {
			showUpgradePrompt = true
			urgencyLevel = 1
		}
	}

	// =====================================================================
	// SIMULATED SAVINGS (for upgrade prompt)
	// Conservative: 15% of COD orders are RTO * avg ₹280 loss per RTO
	// Optimistic:   27% of COD orders are RTO * avg ₹350 loss per RTO
	// =====================================================================
	var codOrderCount int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND execution_mode = ?", merchantID, "SHADOW").
		Count(&codOrderCount) // all shadow orders approximate COD (we see everything)

	simSavingsMin := float64(codOrderCount) * 0.15 * 280.0
	simSavingsMax := float64(codOrderCount) * 0.27 * 350.0
	simSavingsMid := (simSavingsMin + simSavingsMax) / 2.0

	// =====================================================================
	// MODULE ENTITLEMENTS
	// =====================================================================
	hasCrossNetwork := merchant.CrossNetworkActive() // helper method defined in Section 1
	crossNetworkPaywalled := !hasCrossNetwork

	// =====================================================================
	// SECTION A — OWN STORE ANALYTICS
	// =====================================================================
	var ownStore domain.OwnStoreAnalytics
	var crossNetwork domain.CrossNetworkAnalytics
	var rtoEngine domain.RTOEngineAnalytics

	if minCohortMet {
		var err error
		ownStore, err = h.computeOwnStoreAnalytics(ctx, merchantID)
		if err != nil {
			slog.Error("failed to compute own store analytics", "merchant_id", merchantID, "error", err)
			ownStore = domain.OwnStoreAnalytics{}
		}

		// =====================================================================
		// SECTION B — CROSS-NETWORK INTELLIGENCE
		// Full data if hasCrossNetwork, teaser only if not
		// =====================================================================
		crossNetwork, err = h.computeCrossNetworkAnalytics(ctx, merchantID, hasCrossNetwork)
		if err != nil {
			slog.Error("failed to compute cross network analytics", "merchant_id", merchantID, "error", err)
			crossNetwork = domain.CrossNetworkAnalytics{IsTeaserOnly: crossNetworkPaywalled}
		}
		crossNetwork.IsTeaserOnly = crossNetworkPaywalled

		// =====================================================================
		// SECTION C — RTO ENGINE ANALYTICS
		// =====================================================================
		rtoEngine, err = h.computeRTOEngineAnalytics(ctx, merchantID, &merchant, executionMode)
		if err != nil {
			slog.Error("failed to compute rto engine analytics", "merchant_id", merchantID, "error", err)
			rtoEngine = domain.RTOEngineAnalytics{IsSimulated: executionMode == "SHADOW"}
		}
	} else {
		// Teaser and simulated flags must still be populated even when cohort is not met
		crossNetwork.IsTeaserOnly = crossNetworkPaywalled
		rtoEngine.IsSimulated = (executionMode == "SHADOW")
	}

	// =====================================================================
	// ASSEMBLE RESPONSE
	// =====================================================================
	var settings domain.MerchantSettings
	h.pg.WithContext(ctx).Where("merchant_id = ?", merchant.ID).First(&settings)

	tierVal := merchant.Tier
	if tierVal == "" {
		tierVal = domain.TierFree
	}
	capiEnabled := merchant.Tier == domain.TierGrowthAds && settings.MetaCAPIEnabled && settings.MetaPixelID != "" && settings.MetaAccessTokenEncrypted != ""

	// Fetch the most recent BuyerLoyaltySnapshot
	var snapshot domain.BuyerLoyaltySnapshot
	err := h.pg.WithContext(ctx).Where("merchant_id = ?", merchant.ID).
		Order("computed_at DESC").
		First(&snapshot).Error

	var loyaltyInsights domain.BuyerLoyaltyInsights
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			loyaltyInsights = domain.BuyerLoyaltyInsights{
				HasSufficientData:      false,
				InsufficientDataReason: "not_yet_computed",
			}
		} else {
			slog.Error("failed to fetch buyer loyalty snapshot", "merchant_id", merchant.ID, "error", err)
			loyaltyInsights = domain.BuyerLoyaltyInsights{
				HasSufficientData:      false,
				InsufficientDataReason: "not_yet_computed",
			}
		}
	} else {
		loyaltyInsights = BuildBuyerLoyaltyInsights(&snapshot, merchant.Tier)
	}

	statusStr := string(merchant.BackfillStatus)
	if statusStr == "" {
		statusStr = "not_started"
	}

	foundingPeriod := domain.FoundingPeriodInfo{
		Active:              domain.IsFoundingPeriodActive(),
		EndsAt:              domain.FoundingPeriodEndsAt(),
		DaysRemaining:       domain.FoundingPeriodDaysRemaining(),
		AllFeaturesUnlocked: domain.IsFoundingPeriodActive(),
	}

	// =====================================================================
	// SECTION E — CAPI LTV COVERAGE STATS
	// Computed from CAPIEventLog for the current calendar month.
	// Only populated when CAPI is enabled (TierGrowthAds + pixel configured).
	// =====================================================================
	var capiCoverage domain.CAPILTVCoverage
	if capiEnabled {
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		type capiCoverageRow struct {
			PredictionMethod string
			EventCount       int
			AvgSentValue     float64
			AvgRawValue      float64
		}
		var coverageRows []capiCoverageRow
		h.pg.WithContext(ctx).Raw(`
			SELECT
				CASE WHEN prediction_method LIKE 'network_history%' THEN 'ltv' ELSE 'raw' END AS prediction_method,
				COUNT(*) AS event_count,
				AVG(sent_value_inr) AS avg_sent_value,
				AVG(raw_transaction_inr) AS avg_raw_value
			FROM capi_event_logs
			WHERE merchant_id = ? AND sent_at >= ?
			GROUP BY 1
		`, merchantID, monthStart).Scan(&coverageRows)

		for _, r := range coverageRows {
			if r.PredictionMethod == "ltv" {
				capiCoverage.LTVBuyerCount = r.EventCount
			} else {
				capiCoverage.RawBuyerCount = r.EventCount
			}
			capiCoverage.EventsSentMonth += r.EventCount
		}
		total := capiCoverage.LTVBuyerCount + capiCoverage.RawBuyerCount
		if total > 0 {
			capiCoverage.LTVCoveragePct = float64(capiCoverage.LTVBuyerCount) / float64(total) * 100.0
		}

		// Avg multiplier = avg sent value / avg raw value across all LTV events
		type multiplierRow struct {
			AvgSentValue float64
			AvgRawValue  float64
		}
		var mRow multiplierRow
		h.pg.WithContext(ctx).Raw(`
			SELECT AVG(sent_value_inr) AS avg_sent_value, AVG(raw_transaction_inr) AS avg_raw_value
			FROM capi_event_logs
			WHERE merchant_id = ? AND sent_at >= ? AND prediction_method LIKE 'network_history%'
		`, merchantID, monthStart).Scan(&mRow)
		if mRow.AvgRawValue > 0 {
			capiCoverage.AvgLTVMultiplier = mRow.AvgSentValue / mRow.AvgRawValue
		}
	}

	resp := domain.InsightsResponse{
		ExecutionMode:            executionMode,
		TotalOrdersAnalyzed:      int(totalOrdersAnalyzed),
		DataCollectionStartedAt:  merchant.CreatedAt,
		MinCohortMet:             minCohortMet,
		FoundingPeriod:           foundingPeriod,
		ShowUpgradePrompt:        showUpgradePrompt,
		UpgradeUrgencyLevel:      urgencyLevel,
		SimulatedRTOSavingsINR:   simSavingsMid,
		SimulatedSavingsRangeMin: simSavingsMin,
		SimulatedSavingsRangeMax: simSavingsMax,
		HasRTOEngine:             merchant.HasRTOEngine,
		HasPaidSubscription:      domain.IsGrowthOrAbove(merchant.Tier),
		HasCrossNetworkIntel:     domain.IsGrowthOrAbove(merchant.Tier),
		HasCRMUpsellEngine:       domain.IsGrowthOrAbove(merchant.Tier),
		Tier:                     tierVal,
		CapiEnabled:              capiEnabled,
		GrowthMonthlyINR:         domain.GrowthMonthlyPaise / 100,
		GrowthAdsMonthlyINR:      domain.GrowthAdsMonthlyPaise / 100,
		PaidTiersAvailable:      false,
		RTOEngineAvailable:      false,
		WaitlistURL:             "/v1/waitlist/join",
		WaitlistJoined:          checkWaitlistMembership(h.pg, merchant.Email),
		BuyerLoyalty:             loyaltyInsights,
		OwnStore:                 ownStore,
		CrossNetwork:             crossNetwork,
		CrossNetworkPaywalled:    crossNetworkPaywalled,
		RTOEngine:                rtoEngine,
		CAPILTVCoverage:          capiCoverage,
		Backfill: domain.BackfillStats{
			Status:      statusStr,
			OrderCount:  merchant.BackfillOrderCount,
			HorizonAt:   merchant.BackfillHorizonAt,
			CompletedAt: merchant.BackfillCompletedAt,
		},
	}

	return c.JSON(resp)
}

func BuildBuyerLoyaltyInsights(snapshot *domain.BuyerLoyaltySnapshot, tier domain.MerchantTier) domain.BuyerLoyaltyInsights {
	insights := domain.BuyerLoyaltyInsights{
		RepeatRatePct:          snapshot.RepeatRatePct,
		RepeatRateTrendPct:     snapshot.RepeatRateTrendPct,
		HasSufficientData:      snapshot.HasSufficientData,
		InsufficientDataReason: snapshot.InsufficientDataReason,
	}

	if domain.IsGrowthOrAbove(tier) || domain.IsFoundingPeriodActive() {
		trueRepeatRate := snapshot.TrueRepeatRatePct
		trueRepeatRateTrend := snapshot.TrueRepeatRateTrendPct
		shopifyEquiv := snapshot.ShopifyEquivalentRepeatRatePct
		abuserCount := snapshot.RepeatRTOAbuserCount
		abuserTotalRTOs := snapshot.RepeatRTOAbuserTotalRTOs
		abuserEstCost := snapshot.RepeatRTOAbuserEstimatedCostINR

		insights.TrueRepeatRatePct = &trueRepeatRate
		insights.TrueRepeatRateTrendPct = &trueRepeatRateTrend
		if shopifyEquiv != nil {
			shopifyEquivVal := *shopifyEquiv
			insights.ShopifyEquivalentRepeatRatePct = &shopifyEquivVal
		}
		insights.RepeatRTOAbuserCount = &abuserCount
		insights.RepeatRTOAbuserTotalRTOs = &abuserTotalRTOs
		insights.RepeatRTOAbuserEstimatedCostINR = &abuserEstCost
	}

	return insights
}

func (h *AnalyticsHandler) computeOwnStoreAnalytics(ctx context.Context, merchantID string) (domain.OwnStoreAnalytics, error) {
	var result domain.OwnStoreAnalytics

	// --- Volume: last 30 days from OrderAudit ---
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

	// Total orders last 30 days
	var total30 int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND created_at >= ?", merchantID, thirtyDaysAgo).
		Count(&total30)
	result.TotalOrdersLast30Days = int(total30)

	// COD orders: use BillableEvent table (payment_method = 'cod') for active merchants
	// For shadow merchants, approximate COD share from OrderAudit predicted risk score > 0
	// (all shadow orders are ingested regardless of payment method)
	var cod30 int64
	h.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND payment_method = 'cod' AND created_at >= ?", merchantID, thirtyDaysAgo).
		Count(&cod30)
	result.CODOrdersLast30Days = int(cod30)
	if total30 > 0 {
		result.CODShareRate = float64(cod30) / float64(total30)
	}

	// --- Cart value distribution from OrderAudit ---
	// Extract order_value_paise from BillableEvent (more reliable than parsing RawPayload)
	type cartRow struct {
		OrderValuePaise int
	}
	var cartRows []cartRow
	h.pg.WithContext(ctx).
		Model(&domain.BillableEvent{}).
		Select("order_value_paise").
		Where("merchant_id = ? AND order_value_paise > 0", merchantID).
		Scan(&cartRows)

	if len(cartRows) >= 1 {
		values := make([]float64, len(cartRows))
		var sum float64
		for i, r := range cartRows {
			v := float64(r.OrderValuePaise) / 100.0
			values[i] = v
			sum += v
		}
		sort.Float64s(values)
		n := len(values)
		result.AvgCartValueINR = sum / float64(n)
		result.MedianCartValueINR = percentileFromSorted(values, 50)
		result.CartValueP25INR = percentileFromSorted(values, 25)
		result.CartValueP75INR = percentileFromSorted(values, 75)
		result.CartValueP90INR = percentileFromSorted(values, 90)
	}

	// --- RTO rate from TrustProfile rows seen at this merchant ---
	// We aggregate TrustProfile.TotalRTOs and TotalOrders for phones seen at this merchant
	// via BillableEvent.PhoneHash join
	type rtoAgg struct {
		TotalOrders int
		TotalRTOs   int
	}
	var rtoResult rtoAgg
	h.pg.WithContext(ctx).Raw(`
		SELECT
			COALESCE(SUM(tp.total_orders), 0)  AS total_orders,
			COALESCE(SUM(tp.total_rtos), 0)    AS total_rtos
		FROM trust_profiles tp
		INNER JOIN (
			SELECT DISTINCT phone_hash FROM billable_events WHERE merchant_id = ?
		) be ON tp.phone_hash = be.phone_hash
	`, merchantID).Scan(&rtoResult)

	result.ObservedRTOCount = rtoResult.TotalRTOs
	if rtoResult.TotalOrders > 0 {
		result.ObservedRTORate = float64(rtoResult.TotalRTOs) / float64(rtoResult.TotalOrders)
	}
	result.EstimatedRTOCostINR = float64(rtoResult.TotalRTOs) * 280.0 // ₹280 avg fwd+rev shipping

	// --- Buyer intent distribution from OrderAudit.PredictedRiskScore ---
	type scoreRow struct {
		PredictedRiskScore float64
	}
	var scoreRows []scoreRow
	h.pg.WithContext(ctx).
		Model(&domain.OrderAudit{}).
		Select("predicted_risk_score").
		Where("merchant_id = ? AND predicted_risk_score > 0", merchantID).
		Scan(&scoreRows)

	buckets := domain.BuyerIntentBuckets{}
	var scoreSum float64
	for _, r := range scoreRows {
		scoreSum += r.PredictedRiskScore
		switch {
		case r.PredictedRiskScore < 40:
			buckets.HighRiskCount++
		case r.PredictedRiskScore < 70:
			buckets.ModerateCount++
		case r.PredictedRiskScore < 85:
			buckets.TrustedCount++
		default:
			buckets.VIPCount++
		}
	}
	total := len(scoreRows)
	if total > 0 {
		buckets.HighRiskPercent = float64(buckets.HighRiskCount) / float64(total)
		buckets.ModeratePercent = float64(buckets.ModerateCount) / float64(total)
		buckets.TrustedPercent = float64(buckets.TrustedCount) / float64(total)
		buckets.VIPPercent = float64(buckets.VIPCount) / float64(total)
		result.AvgKaughtmanScore = scoreSum / float64(total)
	}
	result.BuyerIntentDistribution = buckets

	switch {
	case result.AvgKaughtmanScore >= 85:
		result.KaughtmanScoreLabel = "VIP"
	case result.AvgKaughtmanScore >= 70:
		result.KaughtmanScoreLabel = "Trusted"
	case result.AvgKaughtmanScore >= 40:
		result.KaughtmanScoreLabel = "Moderate"
	default:
		result.KaughtmanScoreLabel = "High Risk"
	}

	// --- Own store refund/complaint rate from CustomerFeedback ---
	var feedbackCount int64
	h.pg.WithContext(ctx).Model(&domain.CustomerFeedback{}).
		Where("merchant_id = ?", merchantID).Count(&feedbackCount)
	result.OwnStoreRefundCount = int(feedbackCount)
	if total30 > 0 {
		result.OwnStoreRefundRate = float64(feedbackCount) / float64(total30)
	}

	// Top complaint category
	type catRow struct {
		Category string
		Cnt      int
	}
	var topCat catRow
	h.pg.WithContext(ctx).Raw(`
		SELECT category, COUNT(*) AS cnt FROM customer_feedbacks
		WHERE merchant_id = ?
		GROUP BY category ORDER BY cnt DESC LIMIT 1
	`, merchantID).Scan(&topCat)
	result.TopComplaintCategory = topCat.Category

	// --- Top 5 pincodes by order volume ---
	// Pincodes are stored in BillableEvent.RawWebhookBody as JSON.
	// Extract with Postgres JSON operator rather than Go parsing.
	// Only available where raw_webhook_body is not redacted.
	type pincodeRow struct {
		Pincode    string
		OrderCount int
		RTOCount   int
		AvgCart    float64
	}
	var pincodeRows []pincodeRow
	h.pg.WithContext(ctx).Raw(`
		SELECT pincode, order_count, rto_count, avg_cart FROM (
			SELECT
				raw_webhook_body::jsonb->>'shipping_address'->>'zip' AS pincode,
				COUNT(*) AS order_count,
				COALESCE(SUM(CASE WHEN tp.total_rtos > 0 THEN 1 ELSE 0 END), 0) AS rto_count,
				AVG(be.order_value_paise) / 100.0 AS avg_cart
			FROM billable_events be
			LEFT JOIN trust_profiles tp ON tp.phone_hash = be.phone_hash
			WHERE be.merchant_id = ?
			  AND be.raw_webhook_body LIKE '{%'
			  AND be.raw_webhook_body != '[REDACTED]'
			  AND be.raw_webhook_body != '[REDACTED-GDPR-SHOP]'
			  AND be.raw_webhook_body != '[REDACTED-GDPR-CUSTOMER]'
			  AND be.raw_webhook_body::jsonb->>'shipping_address' IS NOT NULL
			GROUP BY pincode
			HAVING COUNT(*) >= 5
		) sub
		WHERE pincode IS NOT NULL AND pincode != ''
		ORDER BY order_count DESC
		LIMIT 5
	`, merchantID).Scan(&pincodeRows)

	for _, pr := range pincodeRows {
		rtoRate := 0.0
		if pr.OrderCount > 0 {
			rtoRate = float64(pr.RTOCount) / float64(pr.OrderCount)
		}
		result.TopPincodesByVolume = append(result.TopPincodesByVolume, domain.PincodeInsight{
			Pincode:    pr.Pincode,
			OrderCount: pr.OrderCount,
			RTORate:    rtoRate,
			AvgCartINR: pr.AvgCart,
		})
	}

	return result, nil
}

// DPDP COMPLIANCE NOTE: Every query in this function operates on AGGREGATE data.
// Minimum cohort floor = 50 distinct phone hashes before any stat is computed.
// No individual phone hash, name, or buyer identifier is ever returned.
func (h *AnalyticsHandler) computeCrossNetworkAnalytics(ctx context.Context, merchantID string, fullAccess bool) (domain.CrossNetworkAnalytics, error) {
	var result domain.CrossNetworkAnalytics

	// --- Check network cohort sufficiency (50+ unique buyers across all merchants) ---
	var totalNetworkBuyers int64
	h.pg.WithContext(ctx).Model(&domain.TrustProfile{}).Count(&totalNetworkBuyers)
	result.NetworkCohortSufficient = totalNetworkBuyers >= 50
	if !result.NetworkCohortSufficient {
		return result, nil
	}

	// Always compute SpendBandDistribution (unlocked under Free Tier)
	type bandRow struct {
		Band string
		Cnt  int
	}
	var bandRows []bandRow
	h.pg.WithContext(ctx).Raw(`
		SELECT
			CASE
				WHEN avg_cart < 50000  THEN 'low'
				WHEN avg_cart < 200000 THEN 'mid'
				WHEN avg_cart < 500000 THEN 'high'
				ELSE 'premium'
			END AS band,
			COUNT(*) AS cnt
		FROM (
			SELECT phone_hash, AVG(order_value_paise) AS avg_cart
			FROM billable_events
			WHERE merchant_id = ? AND order_value_paise > 0
			GROUP BY phone_hash
		) sub
		GROUP BY band
	`, merchantID).Scan(&bandRows)

	var totalBuyers int
	for _, b := range bandRows {
		totalBuyers += b.Cnt
	}
	sbd := domain.SpendBandBreakdown{}
	for _, b := range bandRows {
		pct := 0.0
		if totalBuyers > 0 {
			pct = float64(b.Cnt) / float64(totalBuyers)
		}
		switch b.Band {
		case "low":
			sbd.LowPct = pct
		case "mid":
			sbd.MidPct = pct
		case "high":
			sbd.HighPct = pct
		case "premium":
			sbd.PremiumPct = pct
		}
	}
	result.SpendBandDistribution = sbd

	// If the merchant does not have fullAccess (paid Growth Tier / active trial), exit early
	if !fullAccess {
		result.IsTeaserOnly = true
		return result, nil
	}

	// --- Network-wide cart value stats (from BillableEvent across ALL merchants) ---
	type netStats struct {
		AvgCart    float64
		MedianCart float64
	}
	var ns netStats
	h.pg.WithContext(ctx).Raw(`
		SELECT
			AVG(order_value_paise) / 100.0 AS avg_cart,
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY order_value_paise) / 100.0 AS median_cart
		FROM billable_events
		WHERE order_value_paise > 0
	`).Scan(&ns)
	result.NetworkAvgCartINR = ns.AvgCart
	result.NetworkMedianCartINR = ns.MedianCart

	// --- Top 10% spender avg cart and estimated monthly spend ---
	type top10Row struct {
		AvgCart float64
	}
	var top10 top10Row
	h.pg.WithContext(ctx).Raw(`
		SELECT AVG(order_value_paise) / 100.0 AS avg_cart
		FROM (
			SELECT phone_hash, AVG(order_value_paise) AS order_value_paise
			FROM billable_events
			WHERE order_value_paise > 0
			GROUP BY phone_hash
		) sub
		WHERE order_value_paise >= (
			SELECT PERCENTILE_CONT(0.9) WITHIN GROUP (ORDER BY avg_cart)
			FROM (
				SELECT phone_hash, AVG(order_value_paise) AS avg_cart
				FROM billable_events WHERE order_value_paise > 0
				GROUP BY phone_hash
			) inner_sub
		)
	`).Scan(&top10)
	result.NetworkTop10PctAvgCartINR = top10.AvgCart
	// Estimated monthly: avg_cart * avg order frequency (assume 1.8 orders/month for top buyers)
	result.NetworkTop10PctAvgMonthlyINR = top10.AvgCart * 1.8

	// --- This merchant's spending percentile ---
	var merchantAvgCart float64
	h.pg.WithContext(ctx).Raw(`
		SELECT AVG(order_value_paise) / 100.0
		FROM billable_events WHERE merchant_id = ? AND order_value_paise > 0
	`, merchantID).Scan(&merchantAvgCart)

	var belowCount int64
	h.pg.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM (
			SELECT phone_hash, AVG(order_value_paise) AS avg_cart
			FROM billable_events WHERE order_value_paise > 0
			GROUP BY phone_hash
		) sub WHERE avg_cart < ?
	`, merchantAvgCart*100).Scan(&belowCount)

	if totalNetworkBuyers > 0 {
		result.MerchantSpendingPercentile = (float64(belowCount) / float64(totalNetworkBuyers)) * 100.0
	}

	// --- Buyer overlap: how many of this merchant's phone hashes appear in other merchants' BillableEvents ---
	type overlapRow struct {
		OverlapCount int
		TotalCount   int
	}
	var overlap overlapRow
	h.pg.WithContext(ctx).Raw(`
		SELECT
			COUNT(DISTINCT CASE WHEN other.phone_hash IS NOT NULL THEN mine.phone_hash END) AS overlap_count,
			COUNT(DISTINCT mine.phone_hash) AS total_count
		FROM billable_events mine
		LEFT JOIN (
			SELECT DISTINCT phone_hash FROM billable_events WHERE merchant_id != ?
		) other ON other.phone_hash = mine.phone_hash
		WHERE mine.merchant_id = ?
	`, merchantID, merchantID).Scan(&overlap)

	if overlap.TotalCount > 0 {
		result.NetworkOverlapPct = float64(overlap.OverlapCount) / float64(overlap.TotalCount)
	}

	// Avg monthly spend of overlapping buyers across the full network
	if overlap.OverlapCount >= 50 {
		result.NetworkCohortSufficient = true
		var overlapStats struct {
			AvgCart float64
		}
		h.pg.WithContext(ctx).Raw(`
			SELECT AVG(order_value_paise) / 100.0 AS avg_cart
			FROM billable_events
			WHERE phone_hash IN (
				SELECT DISTINCT phone_hash FROM billable_events WHERE merchant_id = ?
			)
			AND order_value_paise > 0
		`, merchantID).Scan(&overlapStats)
		result.OverlapBuyersAvgCartINR = overlapStats.AvgCart
		result.OverlapBuyersAvgMonthlyINR = overlapStats.AvgCart * 1.5
	} else {
		result.NetworkCohortSufficient = false
	}

	// --- Network-wide RTO and refund rates ---
	type networkRates struct {
		TotalOrders     int
		TotalRTOs       int
		TotalComplaints int
	}
	var rates networkRates
	h.pg.WithContext(ctx).Raw(`
		SELECT
			COALESCE(SUM(total_orders), 0)       AS total_orders,
			COALESCE(SUM(total_rtos), 0)          AS total_rtos
		FROM trust_profiles
	`).Scan(&rates)
	var totalComplaints int64
	h.pg.WithContext(ctx).Model(&domain.CustomerFeedback{}).Count(&totalComplaints)
	rates.TotalComplaints = int(totalComplaints)

	if rates.TotalOrders > 0 {
		result.NetworkRTORateAggregate = float64(rates.TotalRTOs) / float64(rates.TotalOrders)
		result.NetworkRefundRateAggregate = float64(rates.TotalComplaints) / float64(rates.TotalOrders)
	}

	// Merchant RTO vs network delta
	var merchantRTORow struct{ RTORate float64 }
	h.pg.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(tp.total_rtos), 0.0) / NULLIF(SUM(tp.total_orders), 0) AS rto_rate
		FROM trust_profiles tp
		INNER JOIN (
			SELECT DISTINCT phone_hash FROM billable_events WHERE merchant_id = ?
		) be ON tp.phone_hash = be.phone_hash
	`, merchantID).Scan(&merchantRTORow)
	result.MerchantRTOVsNetworkDelta = merchantRTORow.RTORate - result.NetworkRTORateAggregate

	// Kaughtman scores
	var networkAvgScore struct{ Avg float64 }
	h.pg.WithContext(ctx).Raw(`
		SELECT AVG(predicted_risk_score) AS avg FROM order_audits WHERE predicted_risk_score > 0
	`).Scan(&networkAvgScore)
	result.NetworkAvgKaughtmanScore = networkAvgScore.Avg

	var merchantAvgScore struct{ Avg float64 }
	h.pg.WithContext(ctx).Raw(`
		SELECT AVG(predicted_risk_score) AS avg FROM order_audits
		WHERE merchant_id = ? AND predicted_risk_score > 0
	`, merchantID).Scan(&merchantAvgScore)
	result.MerchantAvgKaughtmanScore = merchantAvgScore.Avg

	return result, nil
}

func (h *AnalyticsHandler) computeRTOEngineAnalytics(ctx context.Context, merchantID string, merchant *domain.Merchant, executionMode string) (domain.RTOEngineAnalytics, error) {
	var result domain.RTOEngineAnalytics
	result.IsSimulated = executionMode == "SHADOW"

	// Total orders evaluated (all OrderAudit rows)
	var evaluated int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ?", merchantID).Count(&evaluated)
	result.OrdersEvaluatedTotal = int(evaluated)

	// Orders blocked (predicted_risk_score >= 70, treated as would-block threshold)
	var blocked int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND predicted_risk_score >= 70", merchantID).Count(&blocked)
	result.OrdersBlockedTotal = int(blocked)
	if evaluated > 0 {
		result.BlockRate = float64(blocked) / float64(evaluated)
	}

	// False positive rate: BillableEvents with RequiresReview = true as a share of blocked
	var requiresReview int64
	h.pg.WithContext(ctx).Model(&domain.BillableEvent{}).
		Where("merchant_id = ? AND requires_review = true", merchantID).Count(&requiresReview)
	if blocked > 0 {
		result.FalsePositiveRate = float64(requiresReview) / float64(blocked)
	}

	// Wallet balance (from MerchantSettings)
	var settings domain.MerchantSettings
	if err := h.pg.WithContext(ctx).Where("merchant_id = ?", merchantID).First(&settings).Error; err == nil {
		result.WalletBalanceINR = float64(settings.WalletBalancePaise) / 100.0
	}

	// Avg daily fee and wallet runway
	daysActive := time.Since(merchant.CreatedAt).Hours() / 24
	if daysActive < 1 {
		daysActive = 1
	}
	var totalFeePaise int64
	h.pg.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(fee_paise), 0) FROM billable_events WHERE merchant_id = ?
	`, merchantID).Scan(&totalFeePaise)
	result.AvgDailyFeePaise = int(float64(totalFeePaise) / daysActive)
	result.ProjectedMonthlyFeeINR = float64(result.AvgDailyFeePaise) * 30.0 / 100.0

	if result.AvgDailyFeePaise > 0 {
		walletPaise := int64(settings.WalletBalancePaise)
		result.EstimatedDaysWalletLeft = int(walletPaise / int64(result.AvgDailyFeePaise))
	}

	// RTOs saved and estimated revenue saved
	// Proxy: blocked orders where the TrustProfile for that phone_hash has total_rtos > 0
	var rtosSaved int64
	h.pg.WithContext(ctx).Raw(`
		SELECT COUNT(DISTINCT oa.id)
		FROM order_audits oa
		INNER JOIN trust_profiles tp ON tp.phone_hash = (
			SELECT phone_hash FROM billable_events be
			WHERE be.merchant_id = oa.merchant_id AND be.order_id = oa.order_id LIMIT 1
		)
		WHERE oa.merchant_id = ? AND oa.predicted_risk_score >= 70 AND tp.total_rtos > 0
	`, merchantID).Scan(&rtosSaved)
	result.RTOsSavedTotal = int(rtosSaved)
	result.EstimatedRevenueSavedINR = float64(rtosSaved) * 280.0

	// 3-day trailing
	var trailing3Day int64
	h.pg.WithContext(ctx).Model(&domain.OrderAudit{}).
		Where("merchant_id = ? AND predicted_risk_score >= 70 AND created_at >= NOW() - INTERVAL '3 days'", merchantID).
		Count(&trailing3Day)
	result.ThreeDayTrailingBlocksINR = float64(trailing3Day) * 150.0

	return result, nil
}

// percentileFromSorted returns the value at the given percentile (0–100) from a pre-sorted slice.
func percentileFromSorted(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (pct / 100.0) * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	return sorted[lower] + (idx-float64(lower))*(sorted[upper]-sorted[lower])
}

// Structs for GET /v1/merchants/buyer-intelligence

type QueryResult struct {
	TotalOrdersAnalysed            int     `gorm:"column:total_orders_analysed"`
	EarliestCreatedAt              *string `gorm:"column:earliest_created_at"`
	GhostCount                     int         `gorm:"column:ghost_count"`
	GhostAvgValPaise               float64     `gorm:"column:ghost_avg_val_paise"`
	GhostAvgDaysSinceLastOrder     float64     `gorm:"column:ghost_avg_days_since_last_order"`
	PrepaidCount                   int         `gorm:"column:prepaid_count"`
	PrepaidTotalCodValPaise        float64     `gorm:"column:prepaid_total_cod_val_paise"`
	PrepaidTotalCodOrdersForCohort int         `gorm:"column:prepaid_total_cod_orders_for_cohort"`
	VelocityAvgDaysToReorder       float64     `gorm:"column:velocity_avg_days_to_reorder"`
	VelocityRepeatBuyerCount       int         `gorm:"column:velocity_repeat_buyer_count"`
	VelocitySingleOrderBuyerCount  int         `gorm:"column:velocity_single_order_buyer_count"`
}

type PincodeHealthQueryResult struct {
	Pincode         string  `gorm:"column:pincode"`
	City            string  `gorm:"column:city"`
	State           string  `gorm:"column:state"`
	TotalOrders     int     `gorm:"column:total_orders"`
	CodOrders       int     `gorm:"column:cod_orders"`
	FulfilledCount  int     `gorm:"column:fulfilled_count"`
	FulfillmentRate float64 `gorm:"column:fulfillment_rate"`
}

type BuyerIntelligenceResponse struct {
	Success             bool                        `json:"success"`
	DataAsOf            string                      `json:"data_as_of"`
	TotalOrdersAnalysed int                         `json:"total_orders_analysed"`
	GhostBuyers         GhostBuyersMetrics          `json:"ghost_buyers"`
	PrepaidCandidates   PrepaidConversionMetrics    `json:"prepaid_conversion_candidates"`
	OrderVelocity       OrderVelocityMetrics        `json:"order_velocity"`
	PincodeHealth       []PincodeHealthMetric       `json:"pincode_health"`
}

type GhostBuyersMetrics struct {
	Count                         int     `json:"count"`
	AvgFirstOrderValueINR         float64 `json:"avg_first_order_value_inr"`
	AvgDaysSinceLastOrder         int     `json:"avg_days_since_last_order"`
	RecoverableCount              int     `json:"recoverable_count"`
	RecoverableRevenueEstimateINR float64 `json:"recoverable_revenue_estimate_inr"`
}

type PrepaidConversionMetrics struct {
	Count                       int     `json:"count"`
	AvgOrderValueINR            float64 `json:"avg_order_value_inr"`
	EstimatedMonthlySavingsINR  float64 `json:"estimated_monthly_savings_inr"`
}

type OrderVelocityMetrics struct {
	AvgDaysToReorder       int `json:"avg_days_to_reorder"`
	OptimalWindowStartDay  int `json:"optimal_window_start_day"`
	OptimalWindowEndDay    int `json:"optimal_window_end_day"`
	RepeatBuyerCount       int `json:"repeat_buyer_count"`
	SingleOrderBuyerCount  int `json:"single_order_buyer_count"`
}

type PincodeHealthMetric struct {
	Pincode         string  `json:"pincode"`
	City            string  `json:"city"`
	State           string  `json:"state"`
	TotalOrders     int     `json:"total_orders"`
	CodOrders       int     `json:"cod_orders"`
	FulfilledCount  int     `json:"fulfilled_count"`
	FulfillmentRate float64 `json:"fulfillment_rate"`
	Status          string  `json:"status"`
}

func (h *AnalyticsHandler) GetBuyerIntelligence(c *fiber.Ctx) error {
	merchantIDVal := c.Locals("kaughtman.merchant_id")
	merchantID, ok := merchantIDVal.(string)
	if !ok || merchantID == "" {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": "unauthorized"})
	}

	ctx := c.UserContext()

	// Check Redis cache first
	cacheKey := fmt.Sprintf("buyer_intelligence:%s", merchantID)
	cachedJSON, err := h.redis.Get(ctx, cacheKey).Result()
	if err == nil && cachedJSON != "" {
		c.Set("Content-Type", "application/json")
		return c.SendString(cachedJSON)
	}

	var res QueryResult
	var dbErr error

	nowVal := time.Now().UTC()
	nowMinus30 := nowVal.AddDate(0, 0, -30)

	if h.pg.Dialector.Name() == "sqlite" {
		sqliteQuery := `
			WITH merchant_orders AS (
				SELECT 
					id,
					buyer_phone_normalized,
					buyer_email,
					outcome,
					fulfillment_status,
					payment_method,
					order_value_paise,
					created_at
				FROM orders
				WHERE merchant_id = ?
				  AND buyer_phone_normalized IS NOT NULL
				  AND buyer_phone_normalized != ''
			),
			total_count AS (
				SELECT COUNT(*) AS total_orders_analysed FROM merchant_orders
			),
			earliest_order AS (
				SELECT MIN(created_at) AS earliest_created_at FROM merchant_orders
			),
			ghost_buyer_phones AS (
				SELECT buyer_phone_normalized, MAX(order_value_paise) AS val, MAX(created_at) AS created_at
				FROM merchant_orders
				GROUP BY buyer_phone_normalized
				HAVING COUNT(id) = 1
				   AND MAX(fulfillment_status) = 'fulfilled'
				   AND MAX(created_at) < ?
			),
			ghost_stats AS (
				SELECT 
					COUNT(*) AS count,
					COALESCE(AVG(val), 0) AS avg_val_paise,
					COALESCE(AVG(julianday(?) - julianday(created_at)), 0) AS avg_days_since_last_order
				FROM ghost_buyer_phones
			),
			prepaid_candidates AS (
				SELECT 
					buyer_phone_normalized,
					SUM(CASE WHEN payment_method = 'cod' THEN order_value_paise ELSE 0 END) AS total_cod_val_paise,
					COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) AS cod_order_count
				FROM merchant_orders
				GROUP BY buyer_phone_normalized
				HAVING COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) >= 3
				   AND COUNT(CASE WHEN payment_method = 'cod' AND fulfillment_status != 'fulfilled' THEN 1 END) = 0
				   AND COUNT(CASE WHEN payment_method = 'prepaid' THEN 1 END) = 0
			),
			prepaid_stats AS (
				SELECT 
					COUNT(*) AS count,
					COALESCE(SUM(total_cod_val_paise), 0) AS total_cod_val_paise,
					COALESCE(SUM(cod_order_count), 0) AS total_cod_orders_for_cohort
				FROM prepaid_candidates
			),
			fulfilled_orders_ranked AS (
				SELECT 
					buyer_phone_normalized,
					created_at,
					ROW_NUMBER() OVER (PARTITION BY buyer_phone_normalized ORDER BY created_at ASC) as rn
				FROM merchant_orders
				WHERE fulfillment_status = 'fulfilled'
			),
			reorder_intervals AS (
				SELECT 
					o1.buyer_phone_normalized,
					julianday(o2.created_at) - julianday(o1.created_at) AS days_to_reorder
				FROM fulfilled_orders_ranked o1
				JOIN fulfilled_orders_ranked o2 ON o1.buyer_phone_normalized = o2.buyer_phone_normalized AND o1.rn = 1 AND o2.rn = 2
			),
			repeat_and_single_counts AS (
				SELECT
					COUNT(CASE WHEN count_fulfilled >= 2 THEN 1 END) AS repeat_buyer_count,
					COUNT(CASE WHEN count_fulfilled = 1 THEN 1 END) AS single_order_buyer_count
				FROM (
					SELECT buyer_phone_normalized, COUNT(*) as count_fulfilled
					FROM merchant_orders
					WHERE fulfillment_status = 'fulfilled'
					GROUP BY buyer_phone_normalized
				) t
			),
			velocity_stats AS (
				SELECT 
					COALESCE(AVG(days_to_reorder), 0) AS avg_days_to_reorder
				FROM reorder_intervals
			)
			SELECT 
				(SELECT total_orders_analysed FROM total_count) AS total_orders_analysed,
				(SELECT earliest_created_at FROM earliest_order) AS earliest_created_at,
				(SELECT count FROM ghost_stats) AS ghost_count,
				(SELECT avg_val_paise FROM ghost_stats) AS ghost_avg_val_paise,
				(SELECT avg_days_since_last_order FROM ghost_stats) AS ghost_avg_days_since_last_order,
				(SELECT count FROM prepaid_stats) AS prepaid_count,
				(SELECT total_cod_val_paise FROM prepaid_stats) AS prepaid_total_cod_val_paise,
				(SELECT total_cod_orders_for_cohort FROM prepaid_stats) AS prepaid_total_cod_orders_for_cohort,
				(SELECT avg_days_to_reorder FROM velocity_stats) AS velocity_avg_days_to_reorder,
				(SELECT repeat_buyer_count FROM repeat_and_single_counts) AS velocity_repeat_buyer_count,
				(SELECT single_order_buyer_count FROM repeat_and_single_counts) AS velocity_single_order_buyer_count
		`
		dbErr = h.pg.WithContext(ctx).Raw(sqliteQuery, merchantID, nowMinus30, nowVal).Scan(&res).Error
	} else {
		postgresQuery := `
			WITH merchant_orders AS (
				SELECT 
					id,
					buyer_phone_normalized,
					buyer_email,
					outcome,
					fulfillment_status,
					payment_method,
					order_value_paise,
					created_at
				FROM orders
				WHERE merchant_id = ?::uuid
				  AND buyer_phone_normalized IS NOT NULL
				  AND buyer_phone_normalized != ''
			),
			total_count AS (
				SELECT COUNT(*) AS total_orders_analysed FROM merchant_orders
			),
			earliest_order AS (
				SELECT MIN(created_at) AS earliest_created_at FROM merchant_orders
			),
			ghost_buyer_phones AS (
				SELECT buyer_phone_normalized, MAX(order_value_paise) AS val, MAX(created_at) AS created_at
				FROM merchant_orders
				GROUP BY buyer_phone_normalized
				HAVING COUNT(id) = 1
				   AND MAX(fulfillment_status) = 'fulfilled'
				   AND MAX(created_at) < ?
			),
			ghost_stats AS (
				SELECT 
					COUNT(*) AS count,
					COALESCE(AVG(val), 0) AS avg_val_paise,
					COALESCE(AVG(EXTRACT(EPOCH FROM (? - created_at)) / 86400), 0) AS avg_days_since_last_order
				FROM ghost_buyer_phones
			),
			prepaid_candidates AS (
				SELECT 
					buyer_phone_normalized,
					SUM(CASE WHEN payment_method = 'cod' THEN order_value_paise ELSE 0 END) AS total_cod_val_paise,
					COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) AS cod_order_count
				FROM merchant_orders
				GROUP BY buyer_phone_normalized
				HAVING COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) >= 3
				   AND COUNT(CASE WHEN payment_method = 'cod' AND fulfillment_status != 'fulfilled' THEN 1 END) = 0
				   AND COUNT(CASE WHEN payment_method = 'prepaid' THEN 1 END) = 0
			),
			prepaid_stats AS (
				SELECT 
					COUNT(*) AS count,
					COALESCE(SUM(total_cod_val_paise), 0) AS total_cod_val_paise,
					COALESCE(SUM(cod_order_count), 0) AS total_cod_orders_for_cohort
				FROM prepaid_candidates
			),
			fulfilled_orders_ranked AS (
				SELECT 
					buyer_phone_normalized,
					created_at,
					ROW_NUMBER() OVER (PARTITION BY buyer_phone_normalized ORDER BY created_at ASC) as rn
				FROM merchant_orders
				WHERE fulfillment_status = 'fulfilled'
			),
			reorder_intervals AS (
				SELECT 
					o1.buyer_phone_normalized,
					EXTRACT(EPOCH FROM (o2.created_at - o1.created_at)) / 86400 AS days_to_reorder
				FROM fulfilled_orders_ranked o1
				JOIN fulfilled_orders_ranked o2 ON o1.buyer_phone_normalized = o2.buyer_phone_normalized AND o1.rn = 1 AND o2.rn = 2
			),
			repeat_and_single_counts AS (
				SELECT
					COUNT(CASE WHEN count_fulfilled >= 2 THEN 1 END) AS repeat_buyer_count,
					COUNT(CASE WHEN count_fulfilled = 1 THEN 1 END) AS single_order_buyer_count
				FROM (
					SELECT buyer_phone_normalized, COUNT(*) as count_fulfilled
					FROM merchant_orders
					WHERE fulfillment_status = 'fulfilled'
					GROUP BY buyer_phone_normalized
				) t
			),
			velocity_stats AS (
				SELECT 
					COALESCE(AVG(days_to_reorder), 0) AS avg_days_to_reorder
				FROM reorder_intervals
			)
			SELECT 
				(SELECT total_orders_analysed FROM total_count) AS total_orders_analysed,
				(SELECT earliest_created_at FROM earliest_order) AS earliest_created_at,
				(SELECT count FROM ghost_stats) AS ghost_count,
				(SELECT avg_val_paise FROM ghost_stats) AS ghost_avg_val_paise,
				(SELECT avg_days_since_last_order FROM ghost_stats) AS ghost_avg_days_since_last_order,
				(SELECT count FROM prepaid_stats) AS prepaid_count,
				(SELECT total_cod_val_paise FROM prepaid_stats) AS prepaid_total_cod_val_paise,
				(SELECT total_cod_orders_for_cohort FROM prepaid_stats) AS prepaid_total_cod_orders_for_cohort,
				(SELECT avg_days_to_reorder FROM velocity_stats) AS velocity_avg_days_to_reorder,
				(SELECT repeat_buyer_count FROM repeat_and_single_counts) AS velocity_repeat_buyer_count,
				(SELECT single_order_buyer_count FROM repeat_and_single_counts) AS velocity_single_order_buyer_count
		`
		dbErr = h.pg.WithContext(ctx).Raw(postgresQuery, merchantID, nowMinus30, nowVal).Scan(&res).Error
	}

	if dbErr != nil && !errors.Is(dbErr, gorm.ErrRecordNotFound) {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": dbErr.Error()})
	}

	if res.TotalOrdersAnalysed == 0 {
		resp := BuyerIntelligenceResponse{
			Success:             true,
			DataAsOf:            nowVal.Format(time.RFC3339),
			TotalOrdersAnalysed: 0,
			GhostBuyers: GhostBuyersMetrics{
				Count:                         0,
				AvgFirstOrderValueINR:         0.00,
				AvgDaysSinceLastOrder:         0,
				RecoverableCount:              0,
				RecoverableRevenueEstimateINR: 0.00,
			},
			PrepaidCandidates: PrepaidConversionMetrics{
				Count:                      0,
				AvgOrderValueINR:           0.00,
				EstimatedMonthlySavingsINR: 0.00,
			},
			OrderVelocity: OrderVelocityMetrics{
				AvgDaysToReorder:      0,
				OptimalWindowStartDay: 1,
				OptimalWindowEndDay:   3,
				RepeatBuyerCount:      0,
				SingleOrderBuyerCount: 0,
			},
			PincodeHealth: []PincodeHealthMetric{},
		}

		respJSON, _ := json.Marshal(resp)
		h.redis.Set(ctx, cacheKey, respJSON, 6*time.Hour)

		return c.JSON(resp)
	}

	// Fetch pincode health statistics
	var pincodes []PincodeHealthQueryResult
	var pincodeQuery string

	if h.pg.Dialector.Name() == "sqlite" {
		pincodeQuery = `
			WITH merchant_orders AS (
				SELECT 
					shipping_address_pincode,
					payment_method,
					fulfillment_status,
					city,
					state
				FROM orders
				WHERE merchant_id = ?
				  AND buyer_phone_normalized IS NOT NULL
				  AND buyer_phone_normalized != ''
			),
			pincode_stats AS (
				SELECT 
					shipping_address_pincode AS pincode,
					COUNT(*) AS total_orders,
					COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) AS cod_orders,
					COUNT(CASE WHEN fulfillment_status = 'fulfilled' THEN 1 END) AS fulfilled_count
				FROM merchant_orders
				WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != ''
				GROUP BY shipping_address_pincode
			),
			pincode_city AS (
				SELECT shipping_address_pincode, city
				FROM (
					SELECT shipping_address_pincode, city,
						   ROW_NUMBER() OVER (PARTITION BY shipping_address_pincode ORDER BY COUNT(*) DESC, city ASC) as rn
					FROM merchant_orders
					WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != '' AND city IS NOT NULL AND city != ''
					GROUP BY shipping_address_pincode, city
				) t
				WHERE rn = 1
			),
			pincode_state AS (
				SELECT shipping_address_pincode, state
				FROM (
					SELECT shipping_address_pincode, state,
						   ROW_NUMBER() OVER (PARTITION BY shipping_address_pincode ORDER BY COUNT(*) DESC, state ASC) as rn
					FROM merchant_orders
					WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != '' AND state IS NOT NULL AND state != ''
					GROUP BY shipping_address_pincode, state
				) t
				WHERE rn = 1
			)
			SELECT 
				ps.pincode AS pincode,
				COALESCE(pc.city, '') AS city,
				COALESCE(pst.state, '') AS state,
				ps.total_orders AS total_orders,
				ps.cod_orders AS cod_orders,
				ps.fulfilled_count AS fulfilled_count,
				CASE 
					WHEN ps.total_orders = 0 THEN 0.0
					ELSE CAST(ps.fulfilled_count AS float) / CAST(ps.total_orders AS float)
				END AS fulfillment_rate
			FROM pincode_stats ps
			LEFT JOIN pincode_city pc ON pc.shipping_address_pincode = ps.pincode
			LEFT JOIN pincode_state pst ON pst.shipping_address_pincode = ps.pincode
			WHERE ps.total_orders >= 5
			ORDER BY ps.cod_orders DESC
			LIMIT 15
		`
	} else {
		pincodeQuery = `
			WITH merchant_orders AS (
				SELECT 
					shipping_address_pincode,
					payment_method,
					fulfillment_status,
					city,
					state
				FROM orders
				WHERE merchant_id = ?::uuid
				  AND buyer_phone_normalized IS NOT NULL
				  AND buyer_phone_normalized != ''
			),
			pincode_stats AS (
				SELECT 
					shipping_address_pincode AS pincode,
					COUNT(*) AS total_orders,
					COUNT(CASE WHEN payment_method = 'cod' THEN 1 END) AS cod_orders,
					COUNT(CASE WHEN fulfillment_status = 'fulfilled' THEN 1 END) AS fulfilled_count
				FROM merchant_orders
				WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != ''
				GROUP BY shipping_address_pincode
			),
			pincode_city AS (
				SELECT shipping_address_pincode, city
				FROM (
					SELECT shipping_address_pincode, city,
						   ROW_NUMBER() OVER (PARTITION BY shipping_address_pincode ORDER BY COUNT(*) DESC, city ASC) as rn
					FROM merchant_orders
					WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != '' AND city IS NOT NULL AND city != ''
					GROUP BY shipping_address_pincode, city
				) t
				WHERE rn = 1
			),
			pincode_state AS (
				SELECT shipping_address_pincode, state
				FROM (
					SELECT shipping_address_pincode, state,
						   ROW_NUMBER() OVER (PARTITION BY shipping_address_pincode ORDER BY COUNT(*) DESC, state ASC) as rn
					FROM merchant_orders
					WHERE shipping_address_pincode IS NOT NULL AND shipping_address_pincode != '' AND state IS NOT NULL AND state != ''
					GROUP BY shipping_address_pincode, state
				) t
				WHERE rn = 1
			)
			SELECT 
				ps.pincode AS pincode,
				COALESCE(pc.city, '') AS city,
				COALESCE(pst.state, '') AS state,
				ps.total_orders AS total_orders,
				ps.cod_orders AS cod_orders,
				ps.fulfilled_count AS fulfilled_count,
				CASE 
					WHEN ps.total_orders = 0 THEN 0.0
					ELSE CAST(ps.fulfilled_count AS float) / CAST(ps.total_orders AS float)
				END AS fulfillment_rate
			FROM pincode_stats ps
			LEFT JOIN pincode_city pc ON pc.shipping_address_pincode = ps.pincode
			LEFT JOIN pincode_state pst ON pst.shipping_address_pincode = ps.pincode
			WHERE ps.total_orders >= 5
			ORDER BY ps.cod_orders DESC
			LIMIT 15
		`
	}

	err = h.pg.WithContext(ctx).Raw(pincodeQuery, merchantID).Scan(&pincodes).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Status(500).JSON(fiber.Map{"success": false, "error": err.Error()})
	}

	// Metric 1: ghost buyers
	avgGhostVal := math.Round((res.GhostAvgValPaise/100)*100) / 100
	ghostAvgDays := int(math.Floor(res.GhostAvgDaysSinceLastOrder))
	recoverableCount := int(math.Floor(float64(res.GhostCount) * 0.18))
	recoverableRevEst := math.Round((float64(recoverableCount)*avgGhostVal)*100) / 100

	ghostBuyers := GhostBuyersMetrics{
		Count:                         res.GhostCount,
		AvgFirstOrderValueINR:         avgGhostVal,
		AvgDaysSinceLastOrder:         ghostAvgDays,
		RecoverableCount:              recoverableCount,
		RecoverableRevenueEstimateINR: recoverableRevEst,
	}

	// Metric 2: prepaid conversion candidates
	avgPrepaidVal := 0.0
	if res.PrepaidTotalCodOrdersForCohort > 0 {
		avgPrepaidVal = math.Round(((res.PrepaidTotalCodValPaise/100)/float64(res.PrepaidTotalCodOrdersForCohort))*100) / 100
	}

	var earliestTime time.Time
	if res.EarliestCreatedAt != nil {
		v := *res.EarliestCreatedAt
		formats := []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			time.RFC3339,
		}
		for _, f := range formats {
			if parsed, err := time.Parse(f, v); err == nil {
				earliestTime = parsed
				break
			}
		}
	}
	if earliestTime.IsZero() {
		earliestTime = time.Now().UTC()
	}

	months := math.Floor(time.Since(earliestTime).Hours() / 24 / 30)
	if months < 1 {
		months = 1
	}

	estimatedMonthlySavings := 0.0
	if res.PrepaidCount > 0 {
		estimatedMonthlySavings = math.Floor(float64(res.PrepaidCount)*0.30) * (float64(res.PrepaidTotalCodOrdersForCohort) / months / float64(res.PrepaidCount)) * 200
		estimatedMonthlySavings = math.Round(estimatedMonthlySavings*100) / 100
	}

	prepaidCandidates := PrepaidConversionMetrics{
		Count:                      res.PrepaidCount,
		AvgOrderValueINR:           avgPrepaidVal,
		EstimatedMonthlySavingsINR: estimatedMonthlySavings,
	}

	// Metric 3: order velocity
	avgDaysToReorder := int(math.Round(res.VelocityAvgDaysToReorder))
	optimalStart := avgDaysToReorder - 6
	if optimalStart < 1 {
		optimalStart = 1
	}
	optimalEnd := avgDaysToReorder + 3

	orderVelocity := OrderVelocityMetrics{
		AvgDaysToReorder:      avgDaysToReorder,
		OptimalWindowStartDay: optimalStart,
		OptimalWindowEndDay:   optimalEnd,
		RepeatBuyerCount:      res.VelocityRepeatBuyerCount,
		SingleOrderBuyerCount: res.VelocitySingleOrderBuyerCount,
	}

	// Metric 4: pincode health
	pincodeHealth := make([]PincodeHealthMetric, 0)
	for _, p := range pincodes {
		var status string
		switch {
		case p.FulfillmentRate >= 0.85:
			status = "healthy"
		case p.FulfillmentRate >= 0.70:
			status = "watch"
		case p.FulfillmentRate >= 0.55:
			status = "at_risk"
		default:
			status = "critical"
		}

		pincodeHealth = append(pincodeHealth, PincodeHealthMetric{
			Pincode:         p.Pincode,
			City:            p.City,
			State:           p.State,
			TotalOrders:     p.TotalOrders,
			CodOrders:       p.CodOrders,
			FulfilledCount:  p.FulfilledCount,
			FulfillmentRate: math.Round(p.FulfillmentRate*10000) / 10000,
			Status:          status,
		})
	}

	resp := BuyerIntelligenceResponse{
		Success:             true,
		DataAsOf:            nowVal.Format(time.RFC3339),
		TotalOrdersAnalysed: res.TotalOrdersAnalysed,
		GhostBuyers:         ghostBuyers,
		PrepaidCandidates:   prepaidCandidates,
		OrderVelocity:       orderVelocity,
		PincodeHealth:       pincodeHealth,
	}

	// Cache in Redis for 6 hours
	respJSON, _ := json.Marshal(resp)
	h.redis.Set(ctx, cacheKey, respJSON, 6*time.Hour)

	return c.JSON(resp)
}

func checkWaitlistMembership(db *gorm.DB, email string) bool {
	if email == "" {
		return false
	}
	var count int64
	err := db.Table("waitlist_entries").
		Where("lower(email) = ?", strings.ToLower(email)).
		Limit(1).
		Count(&count).Error
	return err == nil && count > 0
}
