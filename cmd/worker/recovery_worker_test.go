package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendWhatsApp_Interakt_Success(t *testing.T) {
	// Start mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert headers
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Basic test_api_key_123" {
			t.Errorf("expected Authorization header 'Basic test_api_key_123', got %q", authHeader)
		}

		// Read body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}

		var payload struct {
			CountryCode  string `json:"countryCode"`
			PhoneNumber  string `json:"phoneNumber"`
			CallbackData string `json:"callbackData"`
			Type         string `json:"type"`
			Template     struct {
				Name         string   `json:"name"`
				LanguageCode string   `json:"languageCode"`
				BodyValues   []string `json:"bodyValues"`
			} `json:"template"`
		}

		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			t.Fatalf("failed to unmarshal request body: %v", err)
		}

		// Assert stripped phone number
		if payload.PhoneNumber != "9876543210" {
			t.Errorf("expected PhoneNumber to be stripped to '9876543210', got %q", payload.PhoneNumber)
		}
		if payload.CountryCode != "+91" {
			t.Errorf("expected CountryCode '+91', got %q", payload.CountryCode)
		}
		if payload.Template.Name != "kotman_rto_verification" {
			t.Errorf("expected template name 'kotman_rto_verification', got %q", payload.Template.Name)
		}
		if len(payload.Template.BodyValues) != 2 || payload.Template.BodyValues[0] != "TestStore" || payload.Template.BodyValues[1] != "Rs. 500" {
			t.Errorf("expected template bodyValues ['TestStore', 'Rs. 500'], got %v", payload.Template.BodyValues)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result": true}`))
	}))
	defer server.Close()

	// Redirect Interakt API call to mock server
	oldURL := interaktAPIURL
	interaktAPIURL = server.URL
	defer func() { interaktAPIURL = oldURL }()

	// Configure environment for the duration of this test
	t.Setenv("WHATSAPP_PROVIDER", "interakt")
	t.Setenv("INTERAKT_API_KEY", "test_api_key_123")
	t.Setenv("WHATSAPP_TEMPLATE_NAME", "kotman_rto_verification")

	worker := &RecoveryWorker{}
	ctx := context.Background()

	err := worker.sendWhatsApp(ctx, "+919876543210", "hash123", "tmpl", 0, "apiKey", "interakt", "TestStore", "Rs. 500")
	if err != nil {
		t.Fatalf("sendWhatsApp failed: %v", err)
	}
}

func TestSendWhatsApp_Interakt_Timeout(t *testing.T) {
	// Start mock HTTP server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // longer than the 100ms test timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Override URL and Timeout for fast testing
	oldURL := interaktAPIURL
	interaktAPIURL = server.URL
	oldTimeout := whatsappTimeout
	whatsappTimeout = 100 * time.Millisecond // fast timeout
	defer func() {
		interaktAPIURL = oldURL
		whatsappTimeout = oldTimeout
	}()

	// Configure environment
	t.Setenv("WHATSAPP_PROVIDER", "interakt")
	t.Setenv("INTERAKT_API_KEY", "test_api_key_123")
	t.Setenv("WHATSAPP_TEMPLATE_NAME", "kotman_rto_verification")

	worker := &RecoveryWorker{}
	ctx := context.Background()

	start := time.Now()
	err := worker.sendWhatsApp(ctx, "+919876543210", "hash123", "tmpl", 0, "apiKey", "interakt", "TestStore", "Rs. 500")
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "Client.Timeout exceeded") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout/deadline exceeded error, got: %v", err)
	}

	// Verify it timed out within expected bounds (~100ms)
	if duration < 100*time.Millisecond || duration > 250*time.Millisecond {
		t.Errorf("expected timeout to fire around 100ms, actually took %v", duration)
	}
}
