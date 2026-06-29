package crm

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "time"
)

// KotmanRiskEvent is the standardised payload every CRM connector receives.
// The connector's job is to translate this into its own API format.
type KotmanRiskEvent struct {
    PhoneHash     string  // 8-char preview for logging, full hash for CRM custom field
    MerchantID    string
    Template      string  // "STANDARD_CART_RECOVERY" | "VIP_RECOVERY_PROMPTED" etc.
    DiscountValue int     // 0 if no incentive
    RiskScore     float64 // derived from TrustProfile
    RTOCount      int
    IsVIP         bool
    EventTime     time.Time
}

// Connector is the interface every CRM must implement.
type Connector interface {
    // Name returns the CRM identifier for logging.
    Name() string
    // SyncRiskEvent pushes a Kotman risk event into the CRM as a contact
    // property update + triggers the appropriate automation workflow.
    SyncRiskEvent(ctx context.Context, event KotmanRiskEvent) error
}

// httpClient is a package-level client shared by all CRM connectors.
// A single client reuses TCP connections across calls via its Transport pool,
// preventing connection exhaustion under load.
var httpClient = &http.Client{
    Timeout: 10 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:        50,
        MaxIdleConnsPerHost: 10,
        IdleConnTimeout:     90 * time.Second,
    },
}

// maxResponseBody caps how much of a CRM response body we'll read,
// preventing allocation bombs from broken or malicious upstream servers.
const maxResponseBody = 1 << 20 // 1 MB

// NewConnector is the factory — returns the right connector based on provider string.
func NewConnector(provider, apiKey, accountID string) (Connector, error) {
    switch provider {
    case "klaviyo":
        return NewKlaviyoConnector(apiKey), nil
    case "hubspot":
        return NewHubSpotConnector(apiKey), nil
    case "moengage":
        if accountID == "" {
            return nil, fmt.Errorf("moengage requires an account ID")
        }
        return NewMoEngageConnector(apiKey, accountID), nil
    case "webengage":
        if accountID == "" {
            return nil, fmt.Errorf("webengage requires a license code (account ID)")
        }
        return NewWebEngageConnector(apiKey, accountID), nil
    default:
        return nil, fmt.Errorf("unknown CRM provider: %s", provider)
    }
}

// postJSON is a shared HTTP helper used by all connectors.
func postJSON(ctx context.Context, url string, headers map[string]string, body interface{}) error {
    data, err := json.Marshal(body)
    if err != nil {
        return fmt.Errorf("marshal error: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
    if err != nil {
        return fmt.Errorf("request build error: %w", err)
    }

    req.Header.Set("Content-Type", "application/json")
    for k, v := range headers {
        req.Header.Set(k, v)
    }

    resp, err := httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("HTTP error: %w", err)
    }
    // Drain and close: reading the body to EOF allows the Transport to reuse
    // the underlying TCP connection for future requests (HTTP keep-alive).
    // LimitReader caps memory at maxResponseBody to prevent allocation bombs.
    defer func() {
        io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
        resp.Body.Close()
    }()

    if resp.StatusCode >= 400 {
        return fmt.Errorf("CRM rejected request with status %d", resp.StatusCode)
    }
    return nil
}

// patchJSON is used by HubSpot for contact property updates.
func patchJSON(ctx context.Context, url string, headers map[string]string, body interface{}) error {
    data, err := json.Marshal(body)
    if err != nil {
        return fmt.Errorf("marshal error: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(data))
    if err != nil {
        return fmt.Errorf("request build error: %w", err)
    }

    req.Header.Set("Content-Type", "application/json")
    for k, v := range headers {
        req.Header.Set(k, v)
    }

    resp, err := httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("HTTP error: %w", err)
    }
    defer func() {
        io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
        resp.Body.Close()
    }()

    if resp.StatusCode >= 400 {
        return fmt.Errorf("CRM rejected patch with status %d", resp.StatusCode)
    }
    return nil
}

// safeHashPreview returns the first 8 characters of a phone hash for logging,
// guarding against panics when the hash is shorter than 8 characters.
func safeHashPreview(hash string) string {
    if len(hash) < 8 {
        return hash
    }
    return hash[:8]
}

// logCRMResult is shared structured logging for all connector results.
func logCRMResult(connector, merchantID, phoneHashPreview string, err error) {
    if err != nil {
        slog.Error("CRM sync failed",
            "crm", connector,
            "merchant_id", merchantID,
            "hash", phoneHashPreview,
            "error", err,
        )
        return
    }
    slog.Info("CRM sync successful",
        "crm", connector,
        "merchant_id", merchantID,
        "hash", phoneHashPreview,
    )
}