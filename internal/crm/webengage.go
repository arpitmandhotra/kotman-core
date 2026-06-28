package crm

import (
    "context"
    "fmt"
    "time"
)

// WebEngageConnector pushes Kotman risk events to WebEngage's REST API.
// WebEngage is widely used by Indian D2C brands for push notifications,
// in-app messages, and journey automation.
//
// Setup required in WebEngage dashboard:
//   1. Go to Data Platform > Integrations > Rest API to get your license code + API key
//   2. Create custom user attributes: kotman_risk_score, kotman_rto_count,
//      kotman_is_vip in User Data settings
//   3. Create a Journey triggered by the "Kotman Recovery" or "Kotman Feedback" event
// Docs: https://docs.webengage.com/docs/rest-api-getting-started
type WebEngageConnector struct {
    apiKey      string
    licenseCode string // WebEngage license code (accountID field)
    apiBase     string
}

func NewWebEngageConnector(apiKey, licenseCode string) *WebEngageConnector {
    return &WebEngageConnector{
        apiKey:      apiKey,
        licenseCode: licenseCode,
        apiBase:     "https://api.webengage.com/v1/accounts",
    }
}

func (w *WebEngageConnector) Name() string { return "webengage" }

func (w *WebEngageConnector) SyncRiskEvent(ctx context.Context, event KotmanRiskEvent) error {
    headers := map[string]string{
        "Authorization": "Bearer " + w.apiKey,
    }

    // Step 1: Create/update user profile with Kotman attributes
    userPayload := map[string]interface{}{
        "userId": event.PhoneHash,
        "attributes": map[string]interface{}{
            "kotman_risk_score":  event.RiskScore,
            "kotman_rto_count":   event.RTOCount,
            "kotman_is_vip":      event.IsVIP,
            "kotman_merchant_id": event.MerchantID,
        },
    }

    err := postJSON(ctx,
        fmt.Sprintf("%s/%s/users", w.apiBase, w.licenseCode),
        headers,
        userPayload,
    )
    if err != nil {
        logCRMResult(w.Name(), event.MerchantID, event.PhoneHash[:8], err)
        return err
    }

    // Step 2: Track the event to trigger WebEngage Journeys
    eventPayload := map[string]interface{}{
        "userId":    event.PhoneHash,
        "eventName": w.eventName(event.Template),
        "eventTime": event.EventTime.Format(time.RFC3339),
        "eventData": map[string]interface{}{
            "template":       event.Template,
            "discount_value": event.DiscountValue,
            "merchant_id":    event.MerchantID,
        },
    }

    err = postJSON(ctx,
        fmt.Sprintf("%s/%s/events", w.apiBase, w.licenseCode),
        headers,
        eventPayload,
    )

    logCRMResult(w.Name(), event.MerchantID, event.PhoneHash[:8], err)
    return err
}

func (w *WebEngageConnector) eventName(template string) string {
    switch template {
    case "STANDARD_CART_RECOVERY", "VIP_RECOVERY_PROMPTED":
        return "Kotman Recovery"
    case "STANDARD_FEEDBACK_REQUEST", "INCENTIVIZED_VIP_FEEDBACK_COUPON":
        return "Kotman Feedback"
    default:
        return "Kotman Event"
    }
}	