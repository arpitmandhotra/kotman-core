package meta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// buildSignal creates a PredictedLTVSignal for testing.
// If networkOrders < 3 or confidence < 0.65, MetaCAPIValue() will return raw fallback.
func buildSignal(rawINR float64, networkOrders int, confidence float64, predicted90DayINR float64) *domain.PredictedLTVSignal {
	return &domain.PredictedLTVSignal{
		RawTransactionValueINR: rawINR,
		NetworkOrderCount:      networkOrders,
		ConfidenceScore:        confidence,
		Predicted90DayLTVINR:   predicted90DayINR,
	}
}

// ---------------------------------------------------------------------------
// MetaCAPIValue unit tests — no network, no DB
// ---------------------------------------------------------------------------

func TestMetaCAPIValue_RawFallback_InsufficientOrders(t *testing.T) {
	s := buildSignal(1500.0, 2, 0.80, 5000.0) // 2 orders < 3 threshold
	val, method := s.MetaCAPIValue()
	if val != 1500.0 {
		t.Errorf("expected 1500.0, got %f", val)
	}
	if method != "raw_fallback:insufficient_orders" {
		t.Errorf("expected raw_fallback:insufficient_orders, got %s", method)
	}
}

func TestMetaCAPIValue_RawFallback_LowConfidence(t *testing.T) {
	s := buildSignal(1500.0, 5, 0.50, 5000.0) // confidence 0.50 < 0.65
	val, method := s.MetaCAPIValue()
	if val != 1500.0 {
		t.Errorf("expected 1500.0, got %f", val)
	}
	if method != "raw_fallback:low_confidence" {
		t.Errorf("expected raw_fallback:low_confidence, got %s", method)
	}
}

func TestMetaCAPIValue_RawFallback_LTVBelowTransaction(t *testing.T) {
	s := buildSignal(1500.0, 5, 0.80, 1000.0) // predicted < raw
	val, method := s.MetaCAPIValue()
	if val != 1500.0 {
		t.Errorf("expected 1500.0, got %f", val)
	}
	if method != "raw_fallback:ltv_below_transaction" {
		t.Errorf("expected raw_fallback:ltv_below_transaction, got %s", method)
	}
}

func TestMetaCAPIValue_NetworkHistory_NormalCase(t *testing.T) {
	s := buildSignal(1500.0, 5, 0.80, 3000.0) // valid LTV, 2x multiplier
	val, method := s.MetaCAPIValue()
	if val != 3000.0 {
		t.Errorf("expected 3000.0, got %f", val)
	}
	if method != "network_history" {
		t.Errorf("expected network_history, got %s", method)
	}
}

func TestMetaCAPIValue_NetworkHistory_Capped5x(t *testing.T) {
	s := buildSignal(1000.0, 5, 0.80, 8000.0) // 8x would be capped to 5x=5000
	val, method := s.MetaCAPIValue()
	if val != 5000.0 {
		t.Errorf("expected 5000.0 (5x cap), got %f", val)
	}
	if method != "network_history:capped_5x" {
		t.Errorf("expected network_history:capped_5x, got %s", method)
	}
}

func TestMetaCAPIValue_ExactlyAtThreshold(t *testing.T) {
	// Exactly 3 orders, exactly 0.65 confidence — should use LTV
	s := buildSignal(1000.0, 3, 0.65, 2500.0)
	val, method := s.MetaCAPIValue()
	if val != 2500.0 {
		t.Errorf("expected 2500.0, got %f", val)
	}
	if method != "network_history" {
		t.Errorf("expected network_history, got %s", method)
	}
}

func TestIsLTVPrediction(t *testing.T) {
	ltv := buildSignal(1000.0, 5, 0.80, 3000.0)
	if !ltv.IsLTVPrediction() {
		t.Error("expected IsLTVPrediction to be true for network_history signal")
	}

	raw := buildSignal(1000.0, 1, 0.80, 3000.0) // insufficient orders
	if raw.IsLTVPrediction() {
		t.Error("expected IsLTVPrediction to be false for raw_fallback signal")
	}
}

// ---------------------------------------------------------------------------
// HashPhoneForMeta — format normalization
// ---------------------------------------------------------------------------

func TestHashPhoneForMeta_Formats(t *testing.T) {
	formats := []string{
		"9876543210",
		"09876543210",
		"919876543210",
		"0919876543210",
	}

	var lastHash string
	for _, f := range formats {
		h := HashPhoneForMeta(f)
		if lastHash != "" && h != lastHash {
			t.Errorf("Format %s normalized to hash %s, expected %s", f, h, lastHash)
		}
		lastHash = h
	}
}

// ---------------------------------------------------------------------------
// SendPurchaseEvent integration tests with mock HTTP server
// ---------------------------------------------------------------------------

func TestSendPurchaseEvent_AtRiskSkipsCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Meta API server should not have been called for at-risk buyer")
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	signal := buildSignal(1500.0, 0, 0.0, 0.0)
	input := CAPIEventInput{
		MerchantID:     "merch_123",
		PixelID:        "123456",
		AccessToken:    "token_abc",
		OrderID:        "ord_999",
		RawPhone:       "9876543210",
		TrustScore:     25, // At-risk score (< 40)
		IsBlacklisted:  false,
		LTVSignal:      signal,
	}

	_, _, err := client.SendPurchaseEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendPurchaseEvent_BlacklistedSkipsCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Meta API server should not have been called for blacklisted buyer")
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	signal := buildSignal(1500.0, 10, 0.90, 5000.0)
	input := CAPIEventInput{
		MerchantID:    "merch_123",
		PixelID:       "123456",
		AccessToken:   "token_abc",
		OrderID:       "ord_999",
		RawPhone:      "9876543210",
		TrustScore:    90,
		IsBlacklisted: true,
		LTVSignal:     signal,
	}

	_, _, err := client.SendPurchaseEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendPurchaseEvent_NilLTVSignalSkipsCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Meta API server should not have been called when LTVSignal is nil")
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	input := CAPIEventInput{
		MerchantID:    "merch_123",
		PixelID:       "123456",
		AccessToken:   "token_abc",
		OrderID:       "ord_999",
		RawPhone:      "9876543210",
		TrustScore:    90,
		IsBlacklisted: false,
		LTVSignal:     nil, // intentionally nil
	}

	_, _, err := client.SendPurchaseEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendPurchaseEvent_RawFallbackSendsCorrectValue(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events_received": 1, "fbtrace_id": "test_trace_id"}`))
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	// Signal with insufficient orders → raw fallback → value = 1500.00
	signal := buildSignal(1500.0, 1, 0.0, 0.0)

	input := CAPIEventInput{
		MerchantID:     "merch_123",
		PixelID:        "123456",
		AccessToken:    "token_abc",
		OrderID:        "ord_999",
		RawPhone:       "9876543210",
		EventTimestamp: time.Now().Unix(),
		TrustScore:     90,
		IsBlacklisted:  false,
		LTVSignal:      signal,
	}

	_, _, err := client.SendPurchaseEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBody == "" {
		t.Fatal("request was not sent to Meta API mock server")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(capturedBody), &payload); err != nil {
		t.Fatalf("failed to parse captured JSON: %v", err)
	}

	dataList, ok := payload["data"].([]interface{})
	if !ok || len(dataList) == 0 {
		t.Fatal("invalid or empty data list in payload")
	}

	event, ok := dataList[0].(map[string]interface{})
	if !ok {
		t.Fatal("invalid event object in payload")
	}

	customData, ok := event["custom_data"].(map[string]interface{})
	if !ok {
		t.Fatal("custom_data is missing in payload")
	}

	val, ok := customData["value"].(float64)
	if !ok {
		t.Fatal("custom_data.value is missing or not a float64")
	}

	// Raw fallback: value should be 1500.00 (the actual transaction, NOT multiplied)
	if val != 1500.0 {
		t.Errorf("expected raw fallback value 1500.00, got %f", val)
	}
}

func TestSendPurchaseEvent_LTVPredictionSendsLTVValue(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events_received": 1, "fbtrace_id": "test_trace_id"}`))
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	// Signal with 5 network orders, 0.80 confidence, predicted 3000 INR LTV
	signal := buildSignal(1500.0, 5, 0.80, 3000.0)

	input := CAPIEventInput{
		MerchantID:     "merch_123",
		PixelID:        "123456",
		AccessToken:    "token_abc",
		OrderID:        "ord_999",
		RawPhone:       "9876543210",
		EventTimestamp: time.Now().Unix(),
		TrustScore:     90,
		IsBlacklisted:  false,
		LTVSignal:      signal,
	}

	_, _, err := client.SendPurchaseEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBody == "" {
		t.Fatal("request was not sent to Meta API mock server")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(capturedBody), &payload); err != nil {
		t.Fatalf("failed to parse captured JSON: %v", err)
	}

	dataList, ok := payload["data"].([]interface{})
	if !ok || len(dataList) == 0 {
		t.Fatal("invalid or empty data list in payload")
	}

	event, ok := dataList[0].(map[string]interface{})
	if !ok {
		t.Fatal("invalid event object in payload")
	}

	customData, ok := event["custom_data"].(map[string]interface{})
	if !ok {
		t.Fatal("custom_data is missing in payload")
	}

	val, ok := customData["value"].(float64)
	if !ok {
		t.Fatal("custom_data.value is missing or not a float64")
	}

	// Should send the predicted LTV (3000), not the raw transaction (1500)
	if val != 3000.0 {
		t.Errorf("expected LTV value 3000.00, got %f", val)
	}
}

func TestSendPurchaseEvent_LTVCappedAt5x(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events_received": 1}`))
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	// Predicted 8000 on a 1000 transaction — must be capped at 5000 (5x)
	signal := buildSignal(1000.0, 5, 0.80, 8000.0)

	input := CAPIEventInput{
		MerchantID:     "merch_123",
		PixelID:        "123456",
		AccessToken:    "token_abc",
		OrderID:        "ord_999",
		RawPhone:       "9876543210",
		EventTimestamp: time.Now().Unix(),
		TrustScore:     90,
		IsBlacklisted:  false,
		LTVSignal:      signal,
	}

	client.SendPurchaseEvent(context.Background(), input)

	var payload map[string]interface{}
	json.Unmarshal([]byte(capturedBody), &payload)
	dataList := payload["data"].([]interface{})
	event := dataList[0].(map[string]interface{})
	customData := event["custom_data"].(map[string]interface{})
	val := customData["value"].(float64)

	if val != 5000.0 {
		t.Errorf("expected 5x-capped value 5000.00, got %f", val)
	}
}

// NoMultiplierField verifies the old kaughtman_multiplier field is not in the payload.
func TestSendPurchaseEvent_NoMultiplierFieldInPayload(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events_received": 1}`))
	}))
	defer server.Close()

	client := NewCAPIClient()
	client.graphAPIBase = server.URL

	signal := buildSignal(1500.0, 5, 0.80, 3000.0)
	input := CAPIEventInput{
		MerchantID:     "merch_123",
		PixelID:        "123456",
		AccessToken:    "token_abc",
		OrderID:        "ord_999",
		RawPhone:       "9876543210",
		EventTimestamp: time.Now().Unix(),
		TrustScore:     90,
		IsBlacklisted:  false,
		LTVSignal:      signal,
	}
	client.SendPurchaseEvent(context.Background(), input)

	// Confirm that old multiplier fields are not present
	if strings.Contains(capturedBody, "kaughtman_multiplier") {
		t.Error("payload must NOT contain kaughtman_multiplier field")
	}
	if strings.Contains(capturedBody, "kaughtman_score") {
		t.Error("payload must NOT contain kaughtman_score field")
	}
}

func TestUploadVerifiedBuyers_TooSmall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Meta API server should not have been called for too small audience")
	}))
	defer server.Close()

	client := NewAudienceClient()
	client.graphAPIBase = server.URL

	// 49 phone hashes (minimum 50 required)
	hashes := make([]string, 49)
	for i := 0; i < 49; i++ {
		hashes[i] = "hash_val"
	}

	_, err := client.UploadVerifiedBuyers(context.Background(), "act_123", "token_xyz", "Kaughtman Verified Buyers", hashes)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "minimum 50 required") {
		t.Errorf("expected error to contain 'minimum 50 required', got '%v'", err)
	}
}
