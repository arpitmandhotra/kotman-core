package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
)

const dummyPayload = `{
  "id": 123456789,
  "total_price": "1500.00",
  "browser_ip": "127.0.0.1",
  "customer": {
    "phone": "+919876543210"
  },
  "shipping_address": {
    "phone": "+919876543210"
  }
}`

func main() {
	// Read secrets and configurations from the environment with sensible defaults
	secret := os.Getenv("SHOPIFY_API_SECRET")
	if secret == "" {
		secret = "shopify_secret"
	}

	apiKey := os.Getenv("MERCHANT_API_KEY")
	if apiKey == "" {
		apiKey = "sk_live_test_api_key"
	}

	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		webhookURL = "http://localhost:8080/v1/webhooks/shopify/orders"
	}

	payloadBytes := []byte(dummyPayload)

	// Compute HMAC-SHA256 signature
	signature := computeHMAC(payloadBytes, secret)

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		fmt.Printf("failed to create request: %v\n", err)
		os.Exit(1)
	}

	// Set Shopify-specific and Auth headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Hmac-Sha256", signature)
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{}
	fmt.Printf("Sending simulation POST request to %s...\n", webhookURL)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("failed to execute request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("failed to read response body: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Status Code: %d %s\n", resp.StatusCode, resp.Status)
	fmt.Printf("Response Body: %s\n", string(body))
}

func computeHMAC(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
