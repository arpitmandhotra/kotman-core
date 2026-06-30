package woocommerce

import (
	"fmt"
	"net/url"
	"strings"
)

// BuildAuthorizeURL constructs the WooCommerce authorization URL.
// storeURL must be validated as a well-formed https:// URL.
func BuildAuthorizeURL(storeURL, appName, returnURL, callbackURL, merchantID string) (string, error) {
	// Validate storeURL is well-formed https:// URL
	u, err := url.Parse(storeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("invalid store URL: must be a secure https:// URL")
	}

	// Clean trailing slash if present
	base := strings.TrimSuffix(storeURL, "/")

	authURL := fmt.Sprintf("%s/wc-auth/v1/authorize?app_name=%s&scope=read_write&user_id=%s&return_url=%s&callback_url=%s",
		base,
		url.QueryEscape(appName),
		url.QueryEscape(merchantID),
		url.QueryEscape(returnURL),
		url.QueryEscape(callbackURL),
	)

	return authURL, nil
}
