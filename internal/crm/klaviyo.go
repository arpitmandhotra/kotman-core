	package crm

import (
    "context"
    "fmt"
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