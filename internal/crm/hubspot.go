package crm

import (
    "context"
    "fmt"
    "net/url"
)

// HubSpotConnector upserts a contact in HubSpot with Kotman risk properties
// and triggers a workflow via the HubSpot Workflows API.
//
// Setup required in HubSpot:
//   1. Create custom contact properties: kotman_risk_score, kotman_rto_count,
//      kotman_template, kotman_is_vip, kotman_merchant_id
//   2. Create a Workflow triggered when kotman_template is enrolled
// Docs: https://developers.hubspot.com/docs/api/crm/contacts
type HubSpotConnector struct {
    apiKey string
}

func NewHubSpotConnector(apiKey string) *HubSpotConnector {
    return &HubSpotConnector{apiKey: apiKey}
}

func (h *HubSpotConnector) Name() string { return "hubspot" }

func (h *HubSpotConnector) SyncRiskEvent(ctx context.Context, event KotmanRiskEvent) error {
    // HubSpot identifies contacts by email or phone. Since we only have the hash,
    // we upsert by a custom unique property: kotman_phone_hash.
    // The merchant must create this as a unique contact property in their HubSpot portal.

    // Step 1: Upsert contact with risk properties
    upsertPayload := map[string]interface{}{
        "properties": map[string]interface{}{
            "kotman_phone_hash":  event.PhoneHash,
            "kotman_risk_score":  fmt.Sprintf("%.2f", event.RiskScore),
            "kotman_rto_count":   event.RTOCount,
            "kotman_is_vip":      event.IsVIP,
            "kotman_template":    event.Template,
            "kotman_merchant_id": event.MerchantID,
            "kotman_segment":     event.SegmentTag,
        },
        "idProperty": "kotman_phone_hash",
    }

    err := patchJSON(ctx,
        fmt.Sprintf("https://api.hubapi.com/crm/v3/objects/contacts/%s?idProperty=kotman_phone_hash",
            url.PathEscape(event.PhoneHash)),
        map[string]string{
            "Authorization": "Bearer " + h.apiKey,
        },
        upsertPayload,
    )

    // If PATCH fails with 404 (contact doesn't exist yet), create it
    if err != nil {
        createPayload := map[string]interface{}{
            "properties": map[string]interface{}{
                "kotman_phone_hash":  event.PhoneHash,
                "kotman_risk_score":  fmt.Sprintf("%.2f", event.RiskScore),
                "kotman_rto_count":   event.RTOCount,
                "kotman_is_vip":      event.IsVIP,
                "kotman_template":    event.Template,
                "kotman_merchant_id": event.MerchantID,
            },
        }
        err = postJSON(ctx,
            "https://api.hubapi.com/crm/v3/objects/contacts",
            map[string]string{
                "Authorization": "Bearer " + h.apiKey,
            },
            createPayload,
        )
    }

    logCRMResult(h.Name(), event.MerchantID, safeHashPreview(event.PhoneHash), err)
    return err
}