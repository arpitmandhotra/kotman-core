package handlers

import (
	"context"
	"math"
	"sort"
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
	shadowDaysPastExpiry := 0

	if !merchant.HasPaidSubscription {
		if now.After(trialEndsAt) {
			shadowDaysPastExpiry = int(now.Sub(trialEndsAt).Hours() / 24)
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
		Where("merchant_id = ? AND execution_mode = ?", merchantID, domain.ExecutionModeShadow).
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
	resp := domain.InsightsResponse{
		ExecutionMode:            executionMode,
		ShadowDaysRemaining:      shadowDaysRemaining,
		ShadowEndsAt:             trialEndsAt,
		TotalOrdersAnalyzed:      int(totalOrdersAnalyzed),
		DataCollectionStartedAt:  merchant.CreatedAt,
		MinCohortMet:             minCohortMet,
		ShowUpgradePrompt:        showUpgradePrompt,
		UpgradeUrgencyLevel:      urgencyLevel,
		ShadowDaysPastExpiry:     shadowDaysPastExpiry,
		SimulatedRTOSavingsINR:   simSavingsMid,
		SimulatedSavingsRangeMin: simSavingsMin,
		SimulatedSavingsRangeMax: simSavingsMax,
		HasRTOEngine:             merchant.HasRTOEngine,
		HasPaidSubscription:      merchant.HasPaidSubscription,
		HasCrossNetworkIntel:     true, // free for lifetime
		HasCRMUpsellEngine:       true, // free for lifetime
		OwnStore:                 ownStore,
		CrossNetwork:             crossNetwork,
		CrossNetworkPaywalled:    false, // free for lifetime, never paywalled
		RTOEngine:                rtoEngine,
	}

	return c.JSON(resp)
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
	// What % of all phone hashes have a LOWER avg cart than this merchant's avg buyer
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
	`, merchantAvgCart*100).Scan(&belowCount) // merchantAvgCart is INR; sub is paise

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
	if overlap.OverlapCount >= 50 { // DPDP cohort floor
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

	// --- Spend band distribution for this merchant's buyers vs network bands ---
	// Bands based on network cart values, applied to this merchant's buyers
	if fullAccess {
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
