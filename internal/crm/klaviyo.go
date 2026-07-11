	package crm

import (
    "context"
    "fmt"
    "math"
    "time"
    "log/slog"

    "github.com/arpitmandhotra/api-integrator/internal/domain"
)

// KlaviyoConnector pushes Kotman risk signals into Klaviyo as profile properties
// and triggers pre-built flows via the Klaviyo Events API (v2026-02).
//
// Setup required in Klaviyo dashboard:
//   1. Create a Flow triggered by "Kotman Cart Recovery" metric
//   2. Create a Flow triggered by "Kotman Feedback Request" metric
//   3. Add profile properties: kotman_risk_score, kotman_rto_count, kotman_is_vip
type KlaviyoConnector struct {
    apiKey string
}

func NewKlaviyoConnector(apiKey string) *KlaviyoConnector {
    return &KlaviyoConnector{apiKey: apiKey}
}

func (k *KlaviyoConnector) Name() string { return "klaviyo" }

func (k *KlaviyoConnector) SyncRiskEvent(ctx context.Context, event KotmanRiskEvent) error {
    // Klaviyo Events API v2 — creates/updates a profile and fires a metric event.
    // This triggers any Klaviyo Flow listening for the "Kotman*" metric.
    // Docs: https://developers.klaviyo.com/en/reference/create_event
    payload := map[string]interface{}{
        "data": map[string]interface{}{
            "type": "event",
            "attributes": map[string]interface{}{
                "metric": map[string]interface{}{
                    "data": map[string]interface{}{
                        "type": "metric",
                        "attributes": map[string]interface{}{
                            "name": k.metricName(event.Template),
                        },
                    },
                },
                "profile": map[string]interface{}{
                    "data": map[string]interface{}{
                        "type": "profile",
                        "attributes": map[string]interface{}{
                            // Klaviyo identifies by phone in E.164 format
                            // We pass the hash as an external_id since we
                            // don't hold raw phones at sync time
                            "external_id": event.PhoneHash,
                            "properties": map[string]interface{}{
                                "kotman_risk_score":    event.RiskScore,
                                "kotman_rto_count":     event.RTOCount,
                                "kotman_is_vip":        event.IsVIP,
                                "kotman_merchant_id":   event.MerchantID,
                                "kotman_discount_hint": event.DiscountValue,
                                "kotman_segment":       event.SegmentTag,
                            },
                        },
                    },
                },
                "properties": map[string]interface{}{
                    "template":       event.Template,
                    "discount_value": event.DiscountValue,
                },
                "time": event.EventTime.Format("2006-01-02T15:04:05Z"),
            },
        },
    }

    err := postJSON(ctx,
        "https://a.klaviyo.com/api/events/",
        map[string]string{
            "Authorization": "Klaviyo-API-Key " + k.apiKey,
            "revision":      "2024-10-15",
        },
        payload,
    )

    logCRMResult(k.Name(), event.MerchantID, safeHashPreview(event.PhoneHash), err)
    return err
}

func (k *KlaviyoConnector) metricName(template string) string {
    switch template {
    case "STANDARD_CART_RECOVERY", "VIP_RECOVERY_PROMPTED":
        return "Kotman Cart Recovery"
    case "STANDARD_FEEDBACK_REQUEST", "INCENTIVIZED_VIP_FEEDBACK_COUPON":
        return "Kotman Feedback Request"
    default:
        return fmt.Sprintf("Kotman %s", template)
    }
}

// EnrichProfile computes the trust tier and custom properties from the trust profile and updates Klaviyo.
func (k *KlaviyoConnector) EnrichProfile(ctx context.Context, rawPhone string, profile domain.TrustProfile, lastOrderCategory string) error {
    enrichCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    // 1. Calculate Trust Score
    trustScore := 100.0
    rtoRate := 0.0
    if profile.TotalOrders > 0 {
        rtoRate = float64(profile.TotalRTOs) / float64(profile.TotalOrders)
        cancelRate := float64(profile.TotalCancellations) / float64(profile.TotalOrders)
        trustScore -= rtoRate * 60
        trustScore -= cancelRate * 20
        trustScore += profile.RiskAdjustment
    } else {
        trustScore = 85
    }
    if profile.IsBlacklisted {
        trustScore = 5
    }
    if trustScore < 0 {
        trustScore = 0
    }
    if trustScore > 100 {
        trustScore = 100
    }

    // 2. Compute Trust Tier
    var trustTier string
    switch {
    case trustScore >= 80:
        trustTier = "Platinum"
    case trustScore >= 60:
        trustTier = "Gold"
    case trustScore >= 40:
        trustTier = "Silver"
    default:
        trustTier = "At-Risk"
    }

    // 3. COD Reliability
    var codReliability string
    if rtoRate < 0.10 {
        codReliability = "High"
    } else if rtoRate < 0.20 {
        codReliability = "Medium"
    } else {
        codReliability = "Low"
    }

    roundedRtoRate := math.Round(rtoRate*10000) / 10000

    payload := map[string]interface{}{
        "data": map[string]interface{}{
            "type": "profile",
            "attributes": map[string]interface{}{
                "phone_number": rawPhone,
                "properties": map[string]interface{}{
                    "kotman_trust_tier":           trustTier,
                    "kotman_trust_score":          int(math.Round(trustScore)),
                    "kotman_network_rto_rate":     roundedRtoRate,
                    "kotman_total_network_orders": profile.TotalOrders,
                    "kotman_preferred_category":   lastOrderCategory,
                    "kotman_cod_reliability":      codReliability,
                    "kotman_last_enriched":        time.Now().Format("2006-01-02"),
                },
            },
        },
    }

    err := patchJSON(enrichCtx,
        "https://a.klaviyo.com/api/profiles/",
        map[string]string{
            "Authorization": "Klaviyo-API-Key " + k.apiKey,
            "revision":      "2024-10-15",
        },
        payload,
    )
    if err != nil {
        slog.Error("klaviyo: failed to enrich profile", "phone", rawPhone, "error", err)
        return nil
    }

    slog.Info("klaviyo: successfully enriched profile", "phone", rawPhone)
    return nil
}