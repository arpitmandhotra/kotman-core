package meta

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func TestComputeTrustMultiplier_AllBranches(t *testing.T) {
    tests := []struct {
        score         int
        totalOrders   int
        isBlacklisted bool
        expected      float64
        name          string
    }{
        {90, 5, false, 1.50, "VIP with history"},
        {90, 0, false, 1.00, "VIP but new — no history boost"},
        {75, 3, false, 1.25, "Trusted"},
        {62, 8, false, 1.00, "Gold/Allow COD baseline"},
        {50, 2, false, 0.75, "Silver/Casual"},
        {30, 10, false, 0.00, "At-Risk — skip event"},
        {85, 0, false, 1.00, "New buyer default score"},
        {90, 5, true, 0.00, "Blacklisted"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := ComputeTrustMultiplier(tt.score, tt.totalOrders, tt.isBlacklisted)
            if got != tt.expected {
                t.Errorf("ComputeTrustMultiplier(%d, %d, %t) = %f; expected %f", tt.score, tt.totalOrders, tt.isBlacklisted, got, tt.expected)
            }
        })
    }
}

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

func TestSendPurchaseEvent_AtRiskSkipsCall(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        t.Error("Meta API server should not have been called for at-risk buyer")
    }))
    defer server.Close()

    client := NewCAPIClient()
    client.graphAPIBase = server.URL

    input := CAPIEventInput{
        MerchantID:      "merch_123",
        PixelID:         "123456",
        AccessToken:     "token_abc",
        OrderID:         "ord_999",
        RawPhone:        "9876543210",
        OrderValuePaise: 150000,
        TrustScore:      25, // At-risk score (< 40)
        TotalOrders:     5,
        IsBlacklisted:   false,
    }

    err := client.SendPurchaseEvent(context.Background(), input)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}

func TestSendPurchaseEvent_VIPAppliesMultiplier(t *testing.T) {
    var capturedBody string
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        body, err := io.ReadAll(r.Body)
        if err != nil {
            t.Fatalf("failed to read body: %v", err)
        }
        capturedBody = string(body)

        w.Header().Set("Content-Type", "application/json")
        w.Write([]byte(`{"events_received": 1}`))
    }))
    defer server.Close()

    client := NewCAPIClient()
    client.graphAPIBase = server.URL

    input := CAPIEventInput{
        MerchantID:      "merch_123",
        PixelID:         "123456",
        AccessToken:     "token_abc",
        OrderID:         "ord_999",
        RawPhone:        "9876543210",
        OrderValuePaise: 150000, // 1500.00 INR
        TrustScore:      90,     // VIP
        TotalOrders:     5,
        IsBlacklisted:   false,
    }

    err := client.SendPurchaseEvent(context.Background(), input)
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

    expectedVal := 2250.0 // 1500.00 * 1.50 multiplier
    if val != expectedVal {
        t.Errorf("expected custom_data.value to be %f, got %f", expectedVal, val)
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
