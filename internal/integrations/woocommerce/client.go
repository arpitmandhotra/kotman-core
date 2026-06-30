package woocommerce

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// oauthEscape escapes a string according to RFC 3986.
func oauthEscape(s string) string {
	t := url.QueryEscape(s)
	t = strings.ReplaceAll(t, "+", "%20")
	t = strings.ReplaceAll(t, "%7E", "~")
	return t
}

// SignRequest signs a request using OAuth 1.0a one-legged authentication for WooCommerce.
// Returns the fully signed URL containing the OAuth query parameters.
func SignRequest(method, requestURL string, consumerKey, consumerSecret string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}

	// Create random 32-character nonce
	nonceBytes := make([]byte, 16)
	if _, randErr := rand.Read(nonceBytes); randErr != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", randErr)
	}
	nonce := hex.EncodeToString(nonceBytes)

	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	// Gather all parameters
	params := u.Query()
	params.Set("oauth_consumer_key", consumerKey)
	params.Set("oauth_timestamp", timestamp)
	params.Set("oauth_nonce", nonce)
	params.Set("oauth_signature_method", "HMAC-SHA256")

	// Sort parameters alphabetically
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var paramParts []string
	for _, k := range keys {
		// WooCommerce parameters are usually single-valued
		val := params.Get(k)
		paramParts = append(paramParts, fmt.Sprintf("%s=%s", oauthEscape(k), oauthEscape(val)))
	}
	paramString := strings.Join(paramParts, "&")

	// Construct base URL (scheme + host + path)
	// Normalize port if standard
	host := u.Host
	if (u.Scheme == "http" && strings.HasSuffix(host, ":80")) || (u.Scheme == "https" && strings.HasSuffix(host, ":443")) {
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
	}
	baseURL := fmt.Sprintf("%s://%s%s", u.Scheme, host, u.Path)

	// Build Signature Base String
	signatureBase := fmt.Sprintf("%s&%s&%s",
		strings.ToUpper(method),
		oauthEscape(baseURL),
		oauthEscape(paramString),
	)

	// Sign using consumerSecret + "&" as key
	signingKey := oauthEscape(consumerSecret) + "&"
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(signatureBase))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Append signature to query params
	params.Set("oauth_signature", signature)

	// Re-construct the final URL
	u.RawQuery = params.Encode()
	// GORM/net/url QueryEncode escapes space as '+' but WooCommerce wants '%20'
	u.RawQuery = strings.ReplaceAll(u.RawQuery, "+", "%20")

	return u.String(), nil
}
