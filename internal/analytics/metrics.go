package analytics

import (
	"fmt"
	"math"
)

// ============================================================
// METRIC 1: RTO Rate
// What: Percentage of orders that were returned to origin
// Why: Primary measure of COD order quality and buyer intent
// How: (orders with outcome=RTO) / (total orders) × 100
// Edge cases: zero orders returns 0.0 not NaN
// ============================================================
func ComputeRTORate(rtoCount, totalOrders int) float64 {
	if totalOrders == 0 {
		return 0.0
	}
	return math.Round((float64(rtoCount)/float64(totalOrders))*10000) / 100 // 2 decimal places
}

// ============================================================
// METRIC 2: COD Share Rate
// What: Percentage of orders placed as Cash on Delivery
// Why: Higher COD share = higher RTO risk exposure
// How: (COD orders) / (total orders) × 100
// Edge cases: zero orders returns 0.0
// ============================================================
func ComputeCODShareRate(codOrders, totalOrders int) float64 {
	if totalOrders == 0 {
		return 0.0
	}
	return math.Round((float64(codOrders)/float64(totalOrders))*10000) / 100
}

// ============================================================
// METRIC 3: Estimated RTO Cost (INR)
// What: Rupee cost of RTOs in the period
// Why: Converts abstract RTO rate into a money number
// How: rto_count × COST_PER_RTO_INR (₹210 per RTO)
// Edge cases: negative rto_count treated as 0
// ============================================================
const CostPerRTOINR = 210.0

func ComputeEstimatedRTOCostINR(rtoCount int) float64 {
	if rtoCount < 0 {
		rtoCount = 0
	}
	return float64(rtoCount) * CostPerRTOINR
}

// ============================================================
// METRIC 4: Avg Cart Value (INR)
// What: Mean order value across all orders in period
// Why: Context for understanding RTO cost and buyer quality
// How: sum(order_value_paise) / count × 100 to convert to INR
// Edge cases: zero orders returns 0.0
// ============================================================
func ComputeAvgCartValueINR(totalValuePaise int64, orderCount int) float64 {
	if orderCount == 0 || totalValuePaise == 0 {
		return 0.0
	}
	return math.Round((float64(totalValuePaise)/float64(orderCount))/100*100) / 100
}

// ============================================================
// METRIC 5: Percentile Cart Value
// What: Cart value at a given percentile (p25, p75, p90)
// Why: Understand value distribution — important for RTO Engine tier pricing
// How: Sort order values, take value at percentile position
// Edge cases: empty slice returns 0.0; p must be 0-100
// ============================================================
func ComputePercentileINR(sortedValuesPaise []int64, p float64) float64 {
	if len(sortedValuesPaise) == 0 || p < 0 || p > 100 {
		return 0.0
	}
	idx := int(math.Ceil(p/100.0*float64(len(sortedValuesPaise)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sortedValuesPaise) {
		idx = len(sortedValuesPaise) - 1
	}
	return math.Round(float64(sortedValuesPaise[idx])/100*100) / 100
}

// ============================================================
// METRIC 6: Buyer Intent Distribution
// What: Percentage of buyers in each trust tier
// Why: Tells merchant the quality composition of their buyer base
// How: Count buyers in each tier from TrustProfile.BuyerTrustIndex ranges
//      VIP: BTI >= 80, Trusted: BTI 60-79, Moderate: BTI 40-59, High Risk: BTI < 40
// Edge cases: zero total returns all zeros; counts must sum to total
// ============================================================
type IntentDistribution struct {
	VIPCount      int
	TrustedCount  int
	ModerateCount int
	HighRiskCount int
	TotalCount    int
}

func ComputeIntentDistributionPcts(d IntentDistribution) map[string]float64 {
	if d.TotalCount == 0 {
		return map[string]float64{
			"vip_percent":       0.0,
			"trusted_percent":   0.0,
			"moderate_percent":  0.0,
			"high_risk_percent": 0.0,
		}
	}
	total := float64(d.TotalCount)
	round4 := func(v float64) float64 { return math.Round(v*10000) / 10000 }
	return map[string]float64{
		"vip_percent":       round4(float64(d.VIPCount) / total),
		"trusted_percent":   round4(float64(d.TrustedCount) / total),
		"moderate_percent":  round4(float64(d.ModerateCount) / total),
		"high_risk_percent": round4(float64(d.HighRiskCount) / total),
	}
}

// ============================================================
// METRIC 7: Avg Buyer Trust Index (BTI)
// What: Average BTI across all buyers who ordered in period
// Why: Single number summarising buyer quality; fed to AI pipeline
// How: Sum of all buyer BTI scores / count of unique buyers
// Edge cases: zero buyers returns 0.0; BTI values must be 0-100
// ============================================================
func ComputeAvgBuyerTrustIndex(btiScores []int) float64 {
	if len(btiScores) == 0 {
		return 0.0
	}
	sum := 0
	for _, s := range btiScores {
		if s < 0 {
			s = 0
		}
		if s > 100 {
			s = 100
		}
		sum += s
	}
	return math.Round(float64(sum)/float64(len(btiScores))*10) / 10
}

func BTILabel(avgBTI float64) string {
	switch {
	case avgBTI >= 75:
		return "Excellent"
	case avgBTI >= 60:
		return "Good"
	case avgBTI >= 45:
		return "Moderate"
	case avgBTI >= 30:
		return "At Risk"
	default:
		return "Critical"
	}
}

// ============================================================
// METRIC 8: Repeat Rate
// What: % of buyers who placed 2+ orders in this store
// Why: Core retention metric; phone-keyed (not email)
// How: (unique phones with 2+ orders) / (total unique phones) × 100
// Edge cases: zero unique buyers returns 0.0
// ============================================================
func ComputeRepeatRate(repeatBuyers, totalUniqueBuyers int) float64 {
	if totalUniqueBuyers == 0 {
		return 0.0
	}
	return math.Round((float64(repeatBuyers)/float64(totalUniqueBuyers))*10000) / 100
}

// ============================================================
// METRIC 9: True Repeat Rate
// What: % of buyers who repeated AND have zero RTOs network-wide
// Why: Actual loyal buyers — Shopify can't compute this (email keys + no network)
// How: (repeat buyers with network RTO count = 0) / (total unique phones) × 100
// Edge cases: zero unique buyers returns 0.0
// ============================================================
func ComputeTrueRepeatRate(trueRepeatBuyers, totalUniqueBuyers int) float64 {
	if totalUniqueBuyers == 0 {
		return 0.0
	}
	return math.Round((float64(trueRepeatBuyers)/float64(totalUniqueBuyers))*10000) / 100
}

// ============================================================
// METRIC 10: Repeat RTO Abuser Count
// What: Count of buyers with 3+ network orders AND network RTO rate > 40%
// Why: Identifies systematic COD trialling behaviour
// How: COUNT where network_total_orders >= 3 AND (network_rto_count/network_total_orders) > 0.40
// Edge cases: returned as count not rate; estimated cost uses CostPerRTOINR
// ============================================================
func ComputeAbuserEstimatedCostINR(totalAbuserRTOsOnThisMerchant int) float64 {
	if totalAbuserRTOsOnThisMerchant < 0 {
		return 0.0
	}
	return float64(totalAbuserRTOsOnThisMerchant) * CostPerRTOINR
}

// ============================================================
// METRIC 11: Network RTO vs Merchant RTO Delta
// What: Difference between merchant RTO rate and network average
// Why: Contextualises whether merchant's RTO is a them-problem or a market-problem
// How: merchant_rto_rate - network_rto_rate (positive = merchant is worse)
// Edge cases: if network rate is 0 (no data), return 0.0 not a misleading number
// ============================================================
func ComputeRTONetworkDelta(merchantRTORate, networkRTORate float64) float64 {
	if networkRTORate == 0 {
		return 0.0
	}
	return math.Round((merchantRTORate-networkRTORate)*10000) / 100
}

// ============================================================
// METRIC 12: Spending Percentile vs Network
// What: Where this merchant's avg cart value sits in the network distribution
// Why: Tells merchant if their buyers are high-value or low-value vs peers
// How: percentile rank of merchant_avg_cart vs sorted network avg_cart distribution
// Edge cases: empty network returns 50 (neutral/unknown percentile)
// ============================================================
func ComputeSpendingPercentile(merchantAvgCart float64, networkCarts []float64) int {
	if len(networkCarts) == 0 {
		return 50
	}
	below := 0
	for _, v := range networkCarts {
		if v < merchantAvgCart {
			below++
		}
	}
	return int(math.Round(float64(below) / float64(len(networkCarts)) * 100))
}

// ============================================================
// METRIC 13: Predicted 90-Day LTV
// What: Buyer's predicted spend over next 90 days based on network history
// Why: Sent to Meta CAPI as value signal for LTV optimisation
// How: (network_total_spend / months_since_first_order) × 3 × recency_multiplier
// Edge cases:
//   - fewer than 3 network orders → return raw transaction value (not LTV)
//   - confidence < 0.65 → return raw transaction value
//   - predicted LTV < raw transaction → return raw transaction (never reduce)
//   - predicted LTV > 5× raw transaction → cap at 5× (prevent outlier distortion)
//   - months_since_first_order < 0.5 → return raw transaction (too early)
// ============================================================
func ComputePredicted90DayLTVINR(
	networkTotalSpendINR float64,
	monthsSinceFirstOrder float64,
	daysSinceLastOrder int,
	networkOrderCount int,
	confidenceScore float64,
	rawTransactionINR float64,
) (predictedLTV float64, method string) {

	// Hard gate 1: insufficient orders
	if networkOrderCount < 3 {
		return rawTransactionINR, "raw_fallback:insufficient_orders"
	}
	// Hard gate 2: low confidence
	if confidenceScore < 0.65 {
		return rawTransactionINR, "raw_fallback:low_confidence"
	}
	// Hard gate 3: insufficient history window
	if monthsSinceFirstOrder < 0.5 {
		return rawTransactionINR, "raw_fallback:insufficient_history"
	}

	avgMonthlySpend := networkTotalSpendINR / monthsSinceFirstOrder
	raw90DayLTV := avgMonthlySpend * 3.0

	// Recency decay: if buyer hasn't ordered in 90+ days, reduce projection
	recencyMultiplier := 1.0
	if daysSinceLastOrder > 90 {
		decay := math.Exp(-0.01 * float64(daysSinceLastOrder-90))
		recencyMultiplier = math.Max(0.3, decay)
	}
	predicted := raw90DayLTV * recencyMultiplier

	// Hard gate 4: predicted LTV below raw transaction — never reduce
	if predicted < rawTransactionINR {
		return rawTransactionINR, "raw_fallback:ltv_below_transaction"
	}
	// Hard gate 5: cap at 5× raw transaction
	cap := rawTransactionINR * 5.0
	if predicted > cap {
		return cap, "network_history:capped_5x"
	}

	return math.Round(predicted*100) / 100, "network_history"
}

// ============================================================
// METRIC 14: LTV Confidence Score
// What: How reliable our LTV prediction is for this buyer
// Why: Determines whether we send predicted LTV or raw value to Meta
// How: weighted average of order_count_score, recency_score, consistency_score
// Edge cases: scores clamped to [0, 1]; missing std dev uses neutral 0.5
// ============================================================
func ComputeLTVConfidenceScore(
	networkOrderCount int,
	daysSinceLastOrder int,
	orderValueStdDevINR float64,
	avgOrderValueINR float64,
) float64 {
	clamp := func(v float64) float64 {
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return v
	}

	orderCountScore := clamp(float64(networkOrderCount) / 10.0)
	recencyScore := clamp(1.0 - float64(daysSinceLastOrder)/180.0)

	consistencyScore := 0.5 // neutral default when std dev unavailable
	if avgOrderValueINR > 0 && orderValueStdDevINR >= 0 {
		cv := orderValueStdDevINR / avgOrderValueINR
		consistencyScore = clamp(1.0 - math.Min(1.0, cv))
	}

	return math.Round((orderCountScore*0.4+recencyScore*0.4+consistencyScore*0.2)*100) / 100
}

// ============================================================
// MERCHANT SCORE: Operations Score (0–100)
// What: How operationally healthy is this merchant's fulfilment?
// Why: Fed to AI pipeline — captures fulfilment efficiency signals
// How: Weighted sum of sub-signals. All weights sum to 1.0.
// ============================================================
type OperationsScoreInputs struct {
	TotalShipments           int
	DeliveredFirstAttempt    int
	TotalNDROrders           int
	NDRConvertedToRTO        int
	PrepaidShareCurrentMonth float64
	PrepaidShareLastMonth    float64
	OrdersWithValidAddress   int
	TotalOrders              int
	AvgDispatchHours         float64
}

func ComputeOperationsScore(in OperationsScoreInputs) (score int, components []ScoreComponentResult, building bool) {
	if in.TotalShipments == 0 || in.TotalOrders == 0 {
		return 0, nil, true
	}

	type signal struct {
		name   string
		weight float64
		value  float64 // normalised 0–1
		raw    float64 // the actual measured value for display
		good   bool    // true if higher is better
	}

	signals := []signal{}

	// Signal 1: First attempt delivery rate
	fadr := 0.0
	if in.TotalShipments > 0 {
		fadr = float64(in.DeliveredFirstAttempt) / float64(in.TotalShipments)
	}
	signals = append(signals, signal{"First-attempt delivery rate", 0.30, fadr, fadr * 100, true})

	// Signal 2: NDR-to-RTO conversion rate (inverted — lower is better)
	ndrRTORate := 0.0
	if in.TotalNDROrders > 0 {
		ndrRTORate = float64(in.NDRConvertedToRTO) / float64(in.TotalNDROrders)
	}
	signals = append(signals, signal{"NDR recovery rate", 0.25, 1.0 - ndrRTORate, ndrRTORate * 100, false})

	// Signal 3: Prepaid trend
	prepaidTrend := 0.5
	if in.PrepaidShareCurrentMonth > in.PrepaidShareLastMonth+0.02 {
		prepaidTrend = 1.0
	} else if in.PrepaidShareCurrentMonth < in.PrepaidShareLastMonth-0.02 {
		prepaidTrend = 0.0
	}
	signals = append(signals, signal{"Prepaid share trend", 0.20, prepaidTrend, in.PrepaidShareCurrentMonth * 100, true})

	// Signal 4: Address completeness
	addrCompleteness := 0.0
	if in.TotalOrders > 0 {
		addrCompleteness = float64(in.OrdersWithValidAddress) / float64(in.TotalOrders)
	}
	signals = append(signals, signal{"Address completeness", 0.15, addrCompleteness, addrCompleteness * 100, true})

	// Signal 5: Dispatch time
	dispatchScore := 0.1
	switch {
	case in.AvgDispatchHours <= 24:
		dispatchScore = 1.0
	case in.AvgDispatchHours <= 48:
		dispatchScore = 0.7
	case in.AvgDispatchHours <= 72:
		dispatchScore = 0.4
	}
	signals = append(signals, signal{"Average dispatch time", 0.10, dispatchScore, in.AvgDispatchHours, false})

	// Compute weighted score
	weightedSum := 0.0
	for _, s := range signals {
		weightedSum += s.value * s.weight
	}
	rawScore := math.Round(weightedSum * 100)
	if rawScore < 0 {
		rawScore = 0
	}
	if rawScore > 100 {
		rawScore = 100
	}

	components = make([]ScoreComponentResult, len(signals))
	for i, s := range signals {
		dir := DirectionGood
		if s.value < 0.4 {
			dir = DirectionBad
		} else if s.value < 0.7 {
			dir = DirectionNeutral
		}
		components[i] = ScoreComponentResult{
			Name:        s.name,
			Weight:      s.weight,
			RawValue:    s.raw,
			Normalised:  int(math.Round(s.value * 100)),
			Direction:   dir,
			Description: operationsDescription(s.name, s.value, s.raw),
		}
	}

	return int(rawScore), components, false
}

// ============================================================
// MERCHANT SCORE: RTO Efficiency Score (0–100)
// What: Measures how well the merchant manages RTO returns
// ============================================================
type RTOEfficiencyScoreInputs struct {
	MerchantRTORate     float64
	NetworkRTORate      float64
	HighRiskExposure    float64
	CODShare            float64
	NetworkCODShare     float64
	RTOTrendMonths      []float64 // RTO rate for last 3 months
	CategoryRTORates    []float64 // RTO rates per category
}

func ComputeRTOEfficiencyScore(in RTOEfficiencyScoreInputs) (score int, components []ScoreComponentResult, building bool) {
	type signal struct {
		name   string
		weight float64
		value  float64
		raw    float64
	}

	signals := []signal{}
	totalWeight := 0.0

	// Signal 1: RTO vs category baseline (network)
	baselineDiff := in.NetworkRTORate - in.MerchantRTORate
	baselineVal := 0.5 + (baselineDiff / 0.2) // 0.5 is average, better = higher
	if baselineVal < 0 {
		baselineVal = 0
	}
	if baselineVal > 1 {
		baselineVal = 1
	}
	signals = append(signals, signal{"RTO vs category baseline", 0.30, baselineVal, in.MerchantRTORate * 100})
	totalWeight += 0.30

	// Signal 2: High-risk pincode exposure (lower is better)
	riskExpVal := math.Max(0, 1.0-(in.HighRiskExposure/0.5))
	signals = append(signals, signal{"High-risk pincode exposure", 0.25, riskExpVal, in.HighRiskExposure * 100})
	totalWeight += 0.25

	// Signal 3: COD share vs network average (lower COD is better for score)
	codShareDiff := in.NetworkCODShare - in.CODShare
	codShareVal := 0.5 + (codShareDiff / 0.4)
	if codShareVal < 0 {
		codShareVal = 0
	}
	if codShareVal > 1 {
		codShareVal = 1
	}
	signals = append(signals, signal{"COD share vs network", 0.20, codShareVal, in.CODShare * 100})
	totalWeight += 0.20

	// Signal 4: RTO rate trend (3 months)
	trendVal := 0.5
	if len(in.RTOTrendMonths) >= 3 {
		if in.RTOTrendMonths[0] < in.RTOTrendMonths[2]-0.02 {
			trendVal = 1.0 // improving
		} else if in.RTOTrendMonths[0] > in.RTOTrendMonths[2]+0.02 {
			trendVal = 0.0 // worsening
		}
	}
	signals = append(signals, signal{"RTO rate trend (3 months)", 0.15, trendVal, in.MerchantRTORate * 100})
	totalWeight += 0.15

	// Signal 5: Category RTO variance
	if len(in.CategoryRTORates) > 1 {
		mean := 0.0
		for _, r := range in.CategoryRTORates {
			mean += r
		}
		mean /= float64(len(in.CategoryRTORates))
		variance := 0.0
		for _, r := range in.CategoryRTORates {
			variance += math.Pow(r-mean, 2)
		}
		variance /= float64(len(in.CategoryRTORates))
		stdDev := math.Sqrt(variance)
		varianceNorm := math.Max(0, 1.0-(stdDev/0.2))
		signals = append(signals, signal{"Cross-category RTO consistency", 0.10, varianceNorm, stdDev * 100})
		totalWeight += 0.10
	}

	if totalWeight == 0 {
		return 0, nil, true
	}

	// Normalise weights if some signals were skipped
	weightedSum := 0.0
	for _, s := range signals {
		normalised := s.weight / totalWeight
		weightedSum += s.value * normalised
	}

	rawScore := math.Round(weightedSum * 100)
	if rawScore < 0 {
		rawScore = 0
	}
	if rawScore > 100 {
		rawScore = 100
	}

	components = make([]ScoreComponentResult, len(signals))
	for i, s := range signals {
		dir := DirectionGood
		if s.value < 0.4 {
			dir = DirectionBad
		} else if s.value < 0.7 {
			dir = DirectionNeutral
		}
		components[i] = ScoreComponentResult{
			Name:        s.name,
			Weight:      s.weight / totalWeight,
			RawValue:    s.raw,
			Normalised:  int(math.Round(s.value * 100)),
			Direction:   dir,
			Description: rtoEfficiencyDescription(s.name, s.value, s.raw),
		}
	}

	return int(rawScore), components, false
}

// Common types used across score computations
type DirectionResult string

const (
	DirectionGood    DirectionResult = "GOOD"
	DirectionNeutral DirectionResult = "NEUTRAL"
	DirectionBad     DirectionResult = "BAD"
)

type ScoreComponentResult struct {
	Name        string
	Weight      float64
	RawValue    float64
	Normalised  int
	Direction   DirectionResult
	Description string
}

// Description generators — these produce the human-readable text shown in the score breakdown
// and also fed to the AI pipeline as context
func operationsDescription(name string, value, raw float64) string {
	switch name {
	case "First-attempt delivery rate":
		if value >= 0.7 {
			return fmt.Sprintf("%.1f%% of orders delivered on first courier attempt — strong operational performance.", raw)
		}
		if value >= 0.4 {
			return fmt.Sprintf("%.1f%% first-attempt delivery rate — room to improve. Review address validation and courier SLA.", raw)
		}
		return fmt.Sprintf("Only %.1f%% of orders delivered on first attempt. High NDR rates suggest address or courier issues.", raw)
	case "NDR recovery rate":
		ndrRTO := 100 - raw
		if value >= 0.7 {
			return fmt.Sprintf("%.1f%% of failed deliveries are being recovered before becoming RTOs.", 100-ndrRTO)
		}
		return fmt.Sprintf("%.1f%% of non-delivery reports are converting to RTOs. Most failed deliveries are not being rescued.", ndrRTO)
	case "Prepaid share trend":
		if value == 1.0 {
			return fmt.Sprintf("Prepaid share is growing (%.1f%%). This reduces your COD risk exposure month over month.", raw)
		}
		if value == 0.0 {
			return fmt.Sprintf("Prepaid share is declining (%.1f%%). Increasing COD exposure warrants review.", raw)
		}
		return fmt.Sprintf("Prepaid share is stable at %.1f%%.", raw)
	case "Address completeness":
		if value >= 0.95 {
			return fmt.Sprintf("%.1f%% of orders have complete, valid delivery addresses.", raw)
		}
		return fmt.Sprintf("%.1f%% address completeness. Incomplete addresses are a leading cause of avoidable NDRs.", raw)
	case "Average dispatch time":
		if value >= 0.9 {
			return fmt.Sprintf("Average dispatch within %.0f hours — excellent. Faster dispatch correlates with lower RTO.", raw)
		}
		return fmt.Sprintf("Average dispatch takes %.0f hours. Dispatch above 48h increases RTO risk as buyer intent cools.", raw)
	}
	return ""
}

func rtoEfficiencyDescription(name string, value, raw float64) string {
	switch name {
	case "RTO vs category baseline":
		if value >= 0.7 {
			return fmt.Sprintf("Your %.1f%% RTO rate is at or below the baseline for your product category.", raw)
		}
		return fmt.Sprintf("Your %.1f%% RTO rate is above the baseline for your category. This may indicate product listing or sizing issues specific to your brand.", raw)
	case "High-risk pincode exposure":
		if value >= 0.7 {
			return fmt.Sprintf("%.1f%% of your orders go to high-risk pincodes (network RTO >35%%). Your geographic exposure is manageable.", raw)
		}
		return fmt.Sprintf("%.1f%% of your orders are to pincodes where the network-wide RTO rate exceeds 35%%. Consider restricting COD in these zones.", raw)
	case "COD share vs network":
		if value >= 0.7 {
			return fmt.Sprintf("Your %.1f%% COD share is in line with or below the network average.", raw)
		}
		return fmt.Sprintf("Your COD share of %.1f%% is above the network average. Higher COD exposure means more RTO risk — this is partially outside your control.", raw)
	case "RTO rate trend (3 months)":
		if value == 1.0 {
			return "Your RTO rate is improving over the past 3 months — positive trajectory."
		}
		if value == 0.0 {
			return "Your RTO rate is worsening over the past 3 months. Investigate recent SKU or geography changes."
		}
		return "Your RTO rate has been stable over the past 3 months."
	case "Cross-category RTO consistency":
		if value >= 0.7 {
			return "RTO rates are consistent across your product categories — no single category is dragging the overall rate."
		}
		return "Significant RTO variance across categories. One or two categories are likely driving most of your returns."
	}
	return ""
}
