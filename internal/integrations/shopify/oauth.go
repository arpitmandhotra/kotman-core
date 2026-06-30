package shopify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const Scopes = "read_orders,read_customers,write_orders"

type ShopifyTokenResponse struct {
	AccessToken  string `json:"access_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

const maxResponseBody = 1 << 20 // 1 MB

func BuildAuthorizationURL(shop, state string) string {
	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	redirectURI := os.Getenv("SHOPIFY_REDIRECT_URI")
	return fmt.Sprintf("https://%s/admin/oauth/authorize?client_id=%s&scope=%s&redirect_uri=%s&state=%s",
		shop, clientID, url.QueryEscape(Scopes), url.QueryEscape(redirectURI), url.QueryEscape(state))
}

func verifyHMAC(queryParams url.Values, clientSecret string) bool {
	hmacParam := queryParams.Get("hmac")
	if hmacParam == "" {
		return false
	}

	// Remove hmac key
	params := make(map[string][]string)
	for k, v := range queryParams {
		if k != "hmac" {
			params[k] = v
		}
	}

	// Sort alphabetically
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		val := queryParams.Get(k)
		parts = append(parts, fmt.Sprintf("%s=%s", k, val))
	}
	queryString := strings.Join(parts, "&")

	// Calculate HMAC-SHA256
	mac := hmac.New(sha256.New, []byte(clientSecret))
	mac.Write([]byte(queryString))
	expectedMac := mac.Sum(nil)

	actualMac, err := hex.DecodeString(hmacParam)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(actualMac, expectedMac) == 1
}

func VerifyInstallHMAC(queryParams url.Values, clientSecret string) bool {
	return verifyHMAC(queryParams, clientSecret)
}

func VerifyCallbackHMAC(queryParams url.Values, clientSecret string) bool {
	return verifyHMAC(queryParams, clientSecret)
}

func ExchangeCodeForToken(ctx context.Context, shop, code string) (*ShopifyTokenResponse, error) {
	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")

	reqBody := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          code,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	tokenURL := fmt.Sprintf("https://%s/admin/oauth/access_token", shop)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request build error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP error: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Shopify token exchange rejected with status %d", resp.StatusCode)
	}

	var tokenResp ShopifyTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	return &tokenResp, nil
}

func RefreshAccessToken(ctx context.Context, shop, refreshToken string) (*ShopifyTokenResponse, error) {
	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")

	reqBody := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"refresh_token": refreshToken,
		"grant_type":    "refresh_token",
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	tokenURL := fmt.Sprintf("https://%s/admin/oauth/access_token", shop)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request build error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP error: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Shopify token refresh rejected with status %d", resp.StatusCode)
	}

	var tokenResp ShopifyTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	return &tokenResp, nil
}
