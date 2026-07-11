package crm

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "math"
    "net/http"
    "net/url"
    "time"

    "github.com/arpitmandhotra/api-integrator/internal/domain"
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

// EnrichProfile updates contact properties in HubSpot after looking up the contact by phone.
func (h *HubSpotConnector) EnrichProfile(ctx context.Context, rawPhone string, profile domain.TrustProfile, lastOrderCategory string) error {
    enrichCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    // 1. Look up contact by phone
    req, err := http.NewRequestWithContext(enrichCtx, http.MethodGet, "https://api.hubapi.com/crm/v3/objects/contacts?properties=phone", nil)
    if err != nil {
        slog.Error("hubspot: failed to build GET contacts request", "error", err)
        return nil
    }
    req.Header.Set("Authorization", "Bearer "+h.apiKey)
    req.Header.Set("Content-Type", "application/json")

    resp, err := httpClient.Do(req)
    if err != nil {
        slog.Error("hubspot: GET contacts request failed", "error", err)
        return nil
    }
    defer func() {
        io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
        resp.Body.Close()
    }()

    if resp.StatusCode >= 400 {
        slog.Error("hubspot: GET contacts returned error status", "status", resp.StatusCode)
        return nil
    }

    var contactsResp struct {
        Results []struct {
            ID         string `json:"id"`
            Properties struct {
                Phone string `json:"phone"`
            } `json:"properties"`
        } `json:"results"`
    }

    if err := json.NewDecoder(resp.Body).Decode(&contactsResp); err != nil {
        slog.Error("hubspot: failed to decode contacts response", "error", err)
        return nil
    }

    var contactID string
    for _, contact := range contactsResp.Results {
        if contact.Properties.Phone == rawPhone {
            contactID = contact.ID
            break
        }
    }

    if contactID == "" {
        slog.Warn("hubspot: contact not found by phone during enrichment", "phone", rawPhone)
        return nil
    }

    // 2. Calculate values
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

    // Compute Trust Tier
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

    // COD Reliability
    var codReliability string
    if rtoRate < 0.10 {
        codReliability = "High"
    } else if rtoRate < 0.20 {
        codReliability = "Medium"
    } else {
        codReliability = "Low"
    }

    roundedRtoRate := math.Round(rtoRate*10000) / 10000

    // 3. PATCH to /crm/v3/objects/contacts/{id}
    payload := map[string]interface{}{
        "properties": map[string]interface{}{
            "kotman_trust_tier":           trustTier,
            "kotman_trust_score":          fmt.Sprintf("%d", int(math.Round(trustScore))),
            "kotman_network_rto_rate":     fmt.Sprintf("%.4f", roundedRtoRate),
            "kotman_total_network_orders": fmt.Sprintf("%d", profile.TotalOrders),
            "kotman_preferred_category":   lastOrderCategory,
            "kotman_cod_reliability":      codReliability,
            "kotman_last_enriched":        time.Now().Format("2006-01-02"),
        },
    }

    patchURL := fmt.Sprintf("https://api.hubapi.com/crm/v3/objects/contacts/%s", url.PathEscape(contactID))
    err = patchJSON(enrichCtx,
        patchURL,
        map[string]string{
            "Authorization": "Bearer " + h.apiKey,
        },
        payload,
    )
    if err != nil {
        slog.Error("hubspot: failed to patch contact properties", "contact_id", contactID, "error", err)
        return nil
    }

    slog.Info("hubspot: successfully enriched contact profile", "contact_id", contactID, "phone", rawPhone)
    return nil
}