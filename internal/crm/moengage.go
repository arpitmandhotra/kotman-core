package crm

import (
    "context"
    "encoding/base64"
    "fmt"

    "github.com/arpitmandhotra/api-integrator/internal/domain"
)

// MoEngageConnector pushes Kaughtman risk events to MoEngage's Data API.
// MoEngage is dominant among Indian D2C brands (Mamaearth, Boat, Sugar, etc.)
//
// Setup required in MoEngage dashboard:
//   1. Create custom user attributes: kaughtman_risk_score, kaughtman_rto_count,
//      kaughtman_is_vip, kaughtman_merchant_id
//   2. Create a Campaign/Flow triggered by "kaughtman_recovery" or "kaughtman_feedback" event
// Docs: https://developers.moengage.com/hc/en-us/articles/4404910561428
type MoEngageConnector struct {
    apiKey    string
    appID     string // MoEngage App ID (accountID field)
    dataAPIBase string
}

func NewMoEngageConnector(apiKey, appID string) *MoEngageConnector {
    return &MoEngageConnector{
        apiKey:      apiKey,
        appID:       appID,
        dataAPIBase: "https://api-01.moengage.com", // use api-02 for EU region
    }
}

func (m *MoEngageConnector) Name() string { return "moengage" }

func (m *MoEngageConnector) SyncRiskEvent(ctx context.Context, event KaughtmanRiskEvent) error {
    // MoEngage uses Basic Auth: base64(APP_ID:DATA_API_KEY)
    basicAuth := base64.StdEncoding.EncodeToString(
        []byte(m.appID + ":" + m.apiKey),
    )

    // Step 1: Update user attributes (risk profile)
    userPayload := map[string]interface{}{
        "type": "customer",
        "customer_id": event.PhoneHash, // Use hash as stable anonymous ID
        "attributes": map[string]interface{}{
            "kaughtman_risk_score":    event.RiskScore,
            "kaughtman_rto_count":     event.RTOCount,
            "kaughtman_is_vip":        event.IsVIP,
            "kaughtman_merchant_id":   event.MerchantID,
            "kaughtman_last_template": event.Template,
            "kaughtman_segment":       event.SegmentTag,
        },
    }

    err := postJSON(ctx,
        fmt.Sprintf("%s/v1/customer/%s/attribute", m.dataAPIBase, m.appID),
        map[string]string{
            "Authorization": "Basic " + basicAuth,
            "MOE-APPID":     m.appID,
        },
        userPayload,
    )
    if err != nil {
        logCRMResult(m.Name(), event.MerchantID, safeHashPreview(event.PhoneHash), err)
        return err
    }

    // Step 2: Track the event to trigger MoEngage flows
    eventPayload := map[string]interface{}{
        "type": "event",
        "customer_id": event.PhoneHash,
        "actions": []map[string]interface{}{
            {
                "action": m.eventName(event.Template),
                "attributes": map[string]interface{}{
                    "template":       event.Template,
                    "discount_value": event.DiscountValue,
                    "merchant_id":    event.MerchantID,
                },
                "platform":       "web",
                "app_version":    "1.0",
                "current_time":   event.EventTime.Unix(),
            },
        },
    }

    err = postJSON(ctx,
        fmt.Sprintf("%s/v1/customer/%s/event", m.dataAPIBase, m.appID),
        map[string]string{
            "Authorization": "Basic " + basicAuth,
            "MOE-APPID":     m.appID,
        },
        eventPayload,
    )

    logCRMResult(m.Name(), event.MerchantID, safeHashPreview(event.PhoneHash), err)
    return err
}

func (m *MoEngageConnector) eventName(template string) string {
    switch template {
    case "STANDARD_CART_RECOVERY", "VIP_RECOVERY_PROMPTED":
        return "kaughtman_recovery"
    case "STANDARD_FEEDBACK_REQUEST", "INCENTIVIZED_VIP_FEEDBACK_COUPON":
        return "kaughtman_feedback"
    default:
        return "kaughtman_event"
    }
}

func (m *MoEngageConnector) EnrichProfile(ctx context.Context, rawPhone string, profile domain.TrustProfile, lastOrderCategory string) error {
    return nil
}