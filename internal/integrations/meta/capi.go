package meta

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "math"
    "net/http"
    "os"
    "time"
)

type CAPIClient struct {
    httpClient      *http.Client
    graphAPIVersion string
    graphAPIBase    string
}

func NewCAPIClient() *CAPIClient {
    apiBase := os.Getenv("META_GRAPH_API_BASE")
    if apiBase == "" {
        apiBase = "https://graph.facebook.com"
    }
    return &CAPIClient{
        httpClient: &http.Client{
            Timeout: 8 * time.Second,
        },
        graphAPIVersion: "v21.0",
        graphAPIBase:    apiBase,
    }
}

// ComputeTrustMultiplier determines the valuation multiplier based on Kotman trust metrics.
// score >= 85  AND totalOrders > 0  → 1.50  (VIP — confirmed history)
// score >= 70  AND totalOrders > 0  → 1.25  (Trusted)
// score >= 60  AND totalOrders > 0  → 1.00  (Allow COD threshold — baseline)
// score >= 40  AND totalOrders > 0  → 0.75  (Casual — send with discount)
// score >= 85  AND totalOrders == 0 → 1.00  (New buyer, optimistic default 85)
// score < 40                        → 0.00  (At-Risk / High Risk: DO NOT send)
// IsBlacklisted == true             → 0.00  (Hard exclude, no event)
func ComputeTrustMultiplier(score int, totalOrders int, isBlacklisted bool) float64 {
    if isBlacklisted {
        return 0.0
    }
    if score < 40 {
        return 0.0
    }
    if totalOrders == 0 {
        return 1.00
    }
    switch {
    case score >= 85:
        return 1.50
    case score >= 70:
        return 1.25
    case score >= 60:
        return 1.00
    case score >= 40:
        return 0.75
    default:
        return 0.0
    }
}

type CAPIEventInput struct {
    MerchantID       string
    PixelID          string
    AccessToken      string
    TestEventCode    string  // empty in prod
    OrderID          string  // used as event_id for deduplication
    RawPhone         string  // hashed inside this function, never stored raw
    Email            string  // optional, empty string if not available
    CityName         string  // from shipping_address
    OrderValuePaise  int     // the ACTUAL order value — multiplier applied here
    CategoryL1       string  // from BillableEvent (signals subsystem field)
    EventTimestamp   int64   // Unix timestamp of the order
    ShopDomain       string  // for event_source_url
    TrustScore       int     // from TrustProfile evaluation
    TotalOrders      int     // from TrustProfile.TotalOrders
    IsBlacklisted    bool    // from TrustProfile.IsBlacklisted
}

func (c *CAPIClient) SendPurchaseEvent(ctx context.Context, input CAPIEventInput) error {
    multiplier := ComputeTrustMultiplier(input.TrustScore, input.TotalOrders, input.IsBlacklisted)
    if multiplier == 0.0 {
        maskedPhone := ""
        if len(input.RawPhone) > 8 {
            maskedPhone = input.RawPhone[:4] + "****" + input.RawPhone[len(input.RawPhone)-2:]
        } else {
            maskedPhone = "****"
        }
        slog.Info("meta_capi: skipping event for at-risk buyer", "phone", maskedPhone)
        return nil
    }

    weightedValue := (float64(input.OrderValuePaise) / 100.0) * multiplier
    weightedValue = math.Round(weightedValue*100.0) / 100.0
    if weightedValue <= 0.0 {
        slog.Info("meta_capi: skipping event with zero or negative weighted value", "value", weightedValue)
        return nil
    }

    phoneHash := HashPhoneForMeta(input.RawPhone)
    var emailHash string
    if input.Email != "" {
        emailHash = HashForMeta(input.Email)
    }
    cityHash := HashForMeta(input.CityName)
    countryHash := HashForMeta("in") // India, always lowercase per Meta spec

    userData := map[string]interface{}{
        "ph":      []string{phoneHash},
        "ct":      []string{cityHash},
        "country": []string{countryHash},
    }
    if emailHash != "" {
        userData["em"] = []string{emailHash}
    }

    eventData := map[string]interface{}{
        "event_name":       "Purchase",
        "event_time":       input.EventTimestamp,
        "event_id":         input.OrderID,
        "action_source":    "website",
        "event_source_url": "https://" + input.ShopDomain + "/checkout",
        "user_data":        userData,
        "custom_data": map[string]interface{}{
            "value":             weightedValue,
            "currency":          "INR",
            "content_category":  input.CategoryL1,
            "order_id":          input.OrderID,
            "kotman_score":      input.TrustScore,
            "kotman_multiplier": multiplier,
        },
    }

    payload := map[string]interface{}{
        "data": []interface{}{eventData},
    }
    if input.AccessToken != "" {
        payload["access_token"] = input.AccessToken
    }
    if input.TestEventCode != "" {
        payload["test_event_code"] = input.TestEventCode
    }

    url := fmt.Sprintf("%s/%s/%s/events", c.graphAPIBase, c.graphAPIVersion, input.PixelID)
    bodyBytes, err := json.Marshal(payload)
    if err != nil {
        slog.Warn("meta_capi: failed to marshal payload", "error", err)
        return nil
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
    if err != nil {
        slog.Warn("meta_capi: failed to build HTTP request", "error", err)
        return nil
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        slog.Warn("meta_capi: request failed", "error", err)
        return nil
    }
    defer func() {
        io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
        resp.Body.Close()
    }()

    if resp.StatusCode >= 400 {
        respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
        slog.Warn("meta_capi: Meta API returned error", "status", resp.StatusCode, "response", string(respBytes))
        return nil
    }

    var metaResp struct {
        EventsReceived int `json:"events_received"`
    }
    respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
    if err := json.Unmarshal(respBytes, &metaResp); err == nil && metaResp.EventsReceived > 0 {
        pixelLast4 := input.PixelID
        if len(pixelLast4) > 4 {
            pixelLast4 = pixelLast4[len(pixelLast4)-4:]
        }
        slog.Debug("meta_capi: purchase event sent",
            "pixel_id", pixelLast4,
            "order_id", input.OrderID,
            "trust_score", input.TrustScore,
            "multiplier", multiplier,
            "weighted_value_inr", weightedValue,
        )
    } else {
        slog.Warn("meta_capi: unexpected response from Meta API", "response", string(respBytes))
    }

    return nil
}
