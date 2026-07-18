package meta

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

	"github.com/arpitmandhotra/api-integrator/internal/domain"
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

// CAPIEventInput holds all fields needed to send a Meta CAPI Purchase event.
// Value is derived exclusively from a PredictedLTVSignal — never multiplied directly.
type CAPIEventInput struct {
	MerchantID     string
	PixelID        string
	AccessToken    string
	TestEventCode  string  // empty in prod
	OrderID        string  // used as event_id for deduplication
	RawPhone       string  // hashed inside this function, never stored raw
	Email          string  // optional, empty string if not available
	CityName       string  // from shipping_address
	CategoryL1     string  // from BillableEvent (signals subsystem field)
	EventTimestamp int64   // Unix timestamp of the order
	ShopDomain     string  // for event_source_url
	TrustScore     int     // from TrustProfile evaluation
	IsBlacklisted  bool    // from TrustProfile.IsBlacklisted

	// LTV signal — computed before calling SendPurchaseEvent.
	// This is the ONLY source of truth for the value field sent to Meta.
	LTVSignal *domain.PredictedLTVSignal
}

// SendPurchaseEvent dispatches a Meta CAPI Purchase event.
// The value field is derived exclusively from signal.MetaCAPIValue().
// Buyers with TrustScore < 40 or IsBlacklisted are silently skipped.
func (c *CAPIClient) SendPurchaseEvent(ctx context.Context, input CAPIEventInput) (metaEventID string, metaResponseCode int, err error) {
	// Gate: do not send CAPI events for at-risk or blacklisted buyers
	if input.IsBlacklisted || input.TrustScore < 40 {
		maskedPhone := "****"
		if len(input.RawPhone) > 8 {
			maskedPhone = input.RawPhone[:4] + "****" + input.RawPhone[len(input.RawPhone)-2:]
		}
		slog.Info("meta_capi: skipping event for at-risk or blacklisted buyer", "phone", maskedPhone)
		return "", 0, nil
	}

	if input.LTVSignal == nil {
		slog.Warn("meta_capi: LTVSignal is nil, skipping event", "order_id", input.OrderID)
		return "", 0, nil
	}

	// The ONLY place value is determined for Meta CAPI
	capiValue, predictionMethod := input.LTVSignal.MetaCAPIValue()
	if capiValue <= 0.0 {
		slog.Info("meta_capi: skipping event with zero or negative value", "order_id", input.OrderID)
		return "", 0, nil
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
			"value":            capiValue, // predicted LTV or raw fallback — never a multiplied guess
			"currency":         "INR",
			"content_category": input.CategoryL1,
			"order_id":         input.OrderID,
			// Internal audit fields — stripped before sending to Meta
			// These exist only in our log record, NOT in the actual API request
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
		return "", 0, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		slog.Warn("meta_capi: failed to build HTTP request", "error", err)
		return "", 0, nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("meta_capi: request failed", "error", err)
		return "", 0, nil
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 400 {
		slog.Warn("meta_capi: Meta API returned error",
			"status", resp.StatusCode,
			"response", string(respBytes),
			"order_id", input.OrderID,
		)
		return "", resp.StatusCode, nil
	}

	var metaResp struct {
		EventsReceived int    `json:"events_received"`
		FBTraceID      string `json:"fbtrace_id"`
	}
	if jsonErr := json.Unmarshal(respBytes, &metaResp); jsonErr == nil && metaResp.EventsReceived > 0 {
		pixelLast4 := input.PixelID
		if len(pixelLast4) > 4 {
			pixelLast4 = pixelLast4[len(pixelLast4)-4:]
		}
		slog.Debug("meta_capi: purchase event sent",
			"pixel_id", pixelLast4,
			"order_id", input.OrderID,
			"trust_score", input.TrustScore,
			"capi_value_inr", capiValue,
			"prediction_method", predictionMethod,
			"network_orders", input.LTVSignal.NetworkOrderCount,
			"confidence_score", input.LTVSignal.ConfidenceScore,
		)
		return metaResp.FBTraceID, resp.StatusCode, nil
	}

	slog.Warn("meta_capi: unexpected response from Meta API", "response", string(respBytes))
	return "", resp.StatusCode, nil
}
