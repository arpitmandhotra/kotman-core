package ai

import (
	"context"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/analytics"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// AIScorePayload is the structured input sent to the AI model.
// It contains ONLY the three dynamic merchant scores and their breakdowns.
type AIScorePayload struct {
	MerchantID         string          `json:"merchant_id"`
	ComputedAt         time.Time       `json:"computed_at"`
	OperationsScore    AIScoreInput    `json:"operations_score"`
	RTOEfficiencyScore AIScoreInput    `json:"rto_efficiency_score"`
	BuyerQualityScore  AIScoreInput    `json:"buyer_quality_score"`
	MerchantContext    MerchantContext `json:"merchant_context"`
}

type AIScoreInput struct {
	CurrentValue  int                               `json:"current_value"`
	PreviousValue int                               `json:"previous_value"` // last computation
	Trend         string                            `json:"trend"`          // "improving" | "stable" | "worsening"
	Components    []analytics.ScoreComponentResult `json:"components"`
	AIPrompt      string                            `json:"ai_prompt"`
}

type MerchantContext struct {
	DomainCategory      string  `json:"dominant_category"` // e.g. "Fashion", "Electronics"
	GeoTierMix          string  `json:"geo_tier_mix"`      // e.g. "60% Tier3, 30% Tier2, 10% Metro"
	CODShareRate        float64 `json:"cod_share_rate"`
	AvgBuyerTrustIndex  float64 `json:"avg_buyer_trust_index"`
	TotalOrdersAnalysed int     `json:"total_orders_analysed"`
	NetworkPercentile   int     `json:"network_percentile"` // where this merchant sits vs peers
}

// BuildAIPayload constructs the payload to send to the AI model.
func BuildAIPayload(
	merchant *domain.Merchant,
	currentScores map[domain.ScoreType]*domain.MerchantScore,
	previousScores map[domain.ScoreType]*domain.MerchantScore,
	ctxCtx MerchantContext,
) AIScorePayload {

	buildInput := func(scoreType domain.ScoreType) AIScoreInput {
		current := currentScores[scoreType]
		previous := previousScores[scoreType]

		input := AIScoreInput{
			CurrentValue:  0,
			PreviousValue: 0,
			Trend:         "stable",
		}

		if current != nil {
			input.CurrentValue = current.Score
			for _, c := range current.Breakdown {
				input.Components = append(input.Components, analytics.ScoreComponentResult{
					Name:        c.Name,
					Weight:      c.Weight,
					RawValue:    c.RawValue,
					Normalised:  c.NormalizedScore,
					Direction:   analytics.DirectionResult(c.Direction),
					Description: c.Description,
				})
			}
		}

		if previous != nil {
			input.PreviousValue = previous.Score
			delta := input.CurrentValue - input.PreviousValue
			if delta > 3 {
				input.Trend = "improving"
			}
			if delta < -3 {
				input.Trend = "worsening"
			}
		}

		return input
	}

	return AIScorePayload{
		MerchantID:         merchant.ID,
		ComputedAt:         time.Now().UTC(),
		OperationsScore:    buildInput(domain.ScoreOperations),
		RTOEfficiencyScore: buildInput(domain.ScoreRTOEfficiency),
		BuyerQualityScore:  buildInput(domain.ScoreBuyerQuality),
		MerchantContext:    ctxCtx,
	}
}

// SendToAI generates deterministic insights for each score type in the payload.
func SendToAI(ctx context.Context, payload AIScorePayload) (map[domain.ScoreType]string, error) {
	results := make(map[domain.ScoreType]string)

	merchantData := map[string]interface{}{
		"dominant_category":     payload.MerchantContext.DomainCategory,
		"geo_tier_mix":          payload.MerchantContext.GeoTierMix,
		"cod_share_rate":        payload.MerchantContext.CODShareRate,
		"avg_buyer_trust_index": payload.MerchantContext.AvgBuyerTrustIndex,
		"total_orders_analysed": payload.MerchantContext.TotalOrdersAnalysed,
		"network_percentile":    payload.MerchantContext.NetworkPercentile,
	}

	scoreInputs := map[domain.ScoreType]AIScoreInput{
		domain.ScoreOperations:    payload.OperationsScore,
		domain.ScoreRTOEfficiency: payload.RTOEfficiencyScore,
		domain.ScoreBuyerQuality:  payload.BuyerQualityScore,
	}

	for scoreType, input := range scoreInputs {
		if input.CurrentValue == 0 {
			continue // skip if score not computed
		}

		var typeStr string
		switch scoreType {
		case domain.ScoreOperations:
			typeStr = "operations"
		case domain.ScoreRTOEfficiency:
			typeStr = "rto_efficiency"
		case domain.ScoreBuyerQuality:
			typeStr = "buyer_quality"
		default:
			typeStr = "unknown"
		}

		insight := GenerateDeterministicInsight(typeStr, float64(input.CurrentValue), merchantData)
		results[scoreType] = insight
	}

	return results, nil
}

// GenerateDeterministicInsight produces rule-based 2-sentence
// insights without any external API call. This is the current
// production implementation.
//
// REPLACEMENT POINT: When the AI model is ready, replace
// this function's body with a call to the trained model's
// inference endpoint. The function signature must remain
// identical — scoreType, score float64, and merchantData
// map[string]interface{} in, string out. The caller and
// database write are unchanged.
//
// The trained model should receive the same merchantData map
// as its feature input and return a single string of two
// sentences. No other changes to the pipeline are needed.
func GenerateDeterministicInsight(
	scoreType string,
	score float64,
	merchantData map[string]interface{},
) string {
	switch scoreType {
	case "rto_efficiency":
		if score < 40 {
			return "Your RTO rate is critically high — prioritise immediately disabling COD for your top 3 highest-RTO pincodes, which account for the majority of your return losses. Run a prepaid-only campaign for your next flash sale to build a cleaner buyer cohort before re-enabling COD."
		}
		if score >= 40 && score < 65 {
			return "Your RTO rate is above the network average — the fastest lever is applying COD restrictions specifically to first-time buyers in Tier 3 pincodes ordering above your average cart value. Buyers with 2+ fulfilled orders should remain unrestricted to avoid suppressing your loyal COD customers."
		}
		if score >= 65 && score < 80 {
			return "Your RTO efficiency is improving but 20-30% of your COD losses are likely concentrated in one or two product categories — check your pincode health table for the categories with fulfillment rate below 70%. Consider a partial prepaid nudge (5% discount) specifically on those SKUs rather than a blanket COD policy change."
		}
		if score >= 80 {
			return "Your RTO efficiency is strong relative to network benchmarks — focus on sustaining it by monitoring your new buyer cohort's first-order fulfillment rate weekly. If new buyer volume spikes (sale events, influencer drops), temporarily tighten COD thresholds for first-time Tier 3 buyers to protect your score."
		}

	case "buyer_quality":
		if score < 40 {
			return "Your buyer base skews heavily toward at-risk and casual segments — your most urgent action is identifying your ghost buyers (single purchase, 30+ days silent) and running a win-back sequence with a time-limited incentive. Do not run broad discount campaigns until you have separated your high-trust repeat buyers from your one-time COD cohort."
		}
		if score >= 40 && score < 65 {
			return "Your buyer quality is moderate — the highest-impact move is identifying your prepaid conversion candidates (3+ fulfilled COD orders, never prepaid) and offering them a 5% prepaid-exclusive discount on their next order. Converting even 20% of this cohort to prepaid reduces your COD exposure without losing loyal buyers."
		}
		if score >= 65 && score < 80 {
			return "Your buyer quality is healthy but your VIP segment is likely underdeveloped — check what percentage of your repeat buyers have placed 4+ orders versus 2-3 orders. A loyalty tier that unlocks early access or free shipping at the 4th order is the most cost-effective way to pull casual buyers into the trusted segment."
		}
		if score >= 80 {
			return "Your buyer quality is among the strongest in your category network — protect this by ensuring your win-back sequences fire within your store's optimal reorder window (check your order velocity metric) rather than at a generic 7 or 14-day interval. Mis-timed re-engagement is the most common reason high-quality buyer cohorts degrade over 6-12 months."
		}

	case "operations":
		if score < 40 {
			return "Your operations score reflects significant gaps in fulfillment consistency — audit your top 5 COD pincodes for fulfillment rate and identify whether the issue is courier performance, address quality, or product-level defects driving returns. Fixing one root cause systematically will move your score faster than incremental improvements across many fronts."
		}
		if score >= 40 && score < 65 {
			return "Your operational efficiency is below network median — the fastest improvement typically comes from standardising your NDR (non-delivery report) callback process so failed deliveries are re-attempted within 24 hours rather than queued. Check whether your current courier's NDR-to-reattempt window is causing buyers to cancel before the second attempt."
		}
		if score >= 65 && score < 80 {
			return "Your operations are functioning well but your complaint category distribution likely shows SIZE_MISMATCH or NOT_AS_DESCRIBED as your top return driver — these are product listing problems, not logistics problems. Updating your size guide and product images for your top 3 return-generating SKUs will improve your score without any logistics change."
		}
		if score >= 80 {
			return "Your operations score is strong — the main risk at this level is degradation during high-volume events (sales, festivals) when courier SLAs slip and NDR rates spike. Pre-negotiate priority fulfilment SLAs with your primary courier before your next major sale event to protect your score during peak periods."
		}
	}

	return "Review your score breakdown to identify the specific metric with the largest gap from network benchmarks — that metric represents your highest-leverage operational improvement. Focus one operational change per 30-day cycle to measure impact cleanly before layering additional changes."
}
