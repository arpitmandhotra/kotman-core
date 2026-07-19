package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

		// Build the AI-specific prompt for this score
		input.AIPrompt = buildScorePrompt(scoreType, input, ctxCtx)
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

func buildScorePrompt(scoreType domain.ScoreType, input AIScoreInput, ctxCtx MerchantContext) string {
	badComponents := []string{}
	for _, c := range input.Components {
		if c.Direction == analytics.DirectionBad {
			badComponents = append(badComponents, fmt.Sprintf("%s (score: %d/100): %s", c.Name, c.Normalised, c.Description))
		}
	}

	switch scoreType {
	case domain.ScoreOperations:
		return fmt.Sprintf(
			`Operations Score is %d/100 (trend: %s) for a %s merchant with %.0f%% COD share.
Bad signals: %v
Context: %s geo mix, avg buyer trust index %.1f.
Task: In 2 sentences, what is the single most impactful operational change this merchant should make this week? Be specific. Reference the actual numbers.`,
			input.CurrentValue, input.Trend, ctxCtx.DomainCategory, ctxCtx.CODShareRate*100,
			badComponents, ctxCtx.GeoTierMix, ctxCtx.AvgBuyerTrustIndex,
		)

	case domain.ScoreRTOEfficiency:
		return fmt.Sprintf(
			`RTO Efficiency Score is %d/100 (trend: %s) for a %s merchant.
Bad signals: %v
Context: %s geo mix, COD share %.0f%%, network percentile %d.
Task: In 2 sentences, what is the single most impactful action to improve RTO efficiency? Be specific about which pincode, category, or buyer segment to address first.`,
			input.CurrentValue, input.Trend, ctxCtx.DomainCategory,
			badComponents, ctxCtx.GeoTierMix, ctxCtx.CODShareRate*100, ctxCtx.NetworkPercentile,
		)

	case domain.ScoreBuyerQuality:
		return fmt.Sprintf(
			`Buyer Quality Score is %d/100 (trend: %s).
Bad signals: %v
Context: %d total orders analysed, network percentile %d.
Task: In 2 sentences, what is the single most impactful action to improve buyer quality? Be specific about which segment to target.`,
			input.CurrentValue, input.Trend,
			badComponents, ctxCtx.TotalOrdersAnalysed, ctxCtx.NetworkPercentile,
		)
	}
	return ""
}

// SendToAI sends the payload to the AI model and returns its response.
func SendToAI(ctx context.Context, payload AIScorePayload) (map[domain.ScoreType]string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	results := make(map[domain.ScoreType]string)
	scoreInputs := map[domain.ScoreType]AIScoreInput{
		domain.ScoreOperations:    payload.OperationsScore,
		domain.ScoreRTOEfficiency: payload.RTOEfficiencyScore,
		domain.ScoreBuyerQuality:  payload.BuyerQualityScore,
	}

	for scoreType, input := range scoreInputs {
		if input.CurrentValue == 0 || input.AIPrompt == "" {
			continue // skip if score not yet computed
		}

		requestBody := map[string]interface{}{
			"model":      "claude-3-5-sonnet-20241022",
			"max_tokens": 200,
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": input.AIPrompt,
				},
			},
			"system": `You are a senior D2C operations consultant analysing Indian e-commerce merchant data. 
You give precise, actionable recommendations grounded in specific numbers. 
Never use generic advice. Always reference the actual metrics provided. 
Respond in exactly 2 sentences. No preamble. No "I recommend" or "You should". Start directly with the action.`,
		}

		bodyBytes, err := json.Marshal(requestBody)
		if err != nil {
			slog.Error("failed to marshal AI request", "score_type", scoreType, "error", err)
			continue
		}

		resp, err := makeAnthropicRequest(ctx, apiKey, bodyBytes)
		if err != nil {
			slog.Error("AI API call failed", "score_type", scoreType, "error", err)
			continue
		}

		results[scoreType] = resp
	}

	return results, nil
}

func makeAnthropicRequest(ctx context.Context, apiKey string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("anthropic api returned error status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return "", err
	}

	if err := json.Unmarshal(respBytes, &result); err != nil {
		return "", err
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("no content in response")
	}

	return result.Content[0].Text, nil
}
