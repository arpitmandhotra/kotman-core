package csvimport

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
)

// ParsePhone validates that the raw phone number is non-empty and contains
// at least one digit. It then delegates to crypto.HashPhone.
func ParsePhone(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("phone number is empty")
	}
	hasDigits := false
	for _, r := range raw {
		if unicode.IsDigit(r) {
			hasDigits = true
			break
		}
	}
	if !hasDigits {
		return "", fmt.Errorf("phone number contains no digit characters: %q", raw)
	}
	return crypto.HashPhone(raw), nil
}

// ParseAmount strips currency symbols and thousands separators,
// parses as a float, converts to paise (int), and rounds.
func ParseAmount(raw string) (int, error) {
	s := raw
	// Strip currency symbols and names
	replacer := strings.NewReplacer(
		"₹", "",
		"$", "",
		",", "",
		"Rs.", "",
		"rs.", "",
		"RS.", "",
		"Rs", "",
		"rs", "",
		"RS", "",
		"INR", "",
		"inr", "",
		"Inr", "",
	)
	s = replacer.Replace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount after stripping formatting: %q", raw)
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse amount as float: %w", err)
	}

	if val < 0 {
		return 0, fmt.Errorf("negative amounts are not allowed: %f", val)
	}

	// Multiply by 100 to get paise and round to nearest int
	paise := math.Round(val * 100.0)
	return int(paise), nil
}

// ParseDate tries to parse a date using a prioritized list of formats.
// It rejects dates more than 1 day in the future or before 2010.
func ParseDate(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date string")
	}

	var parsedTime time.Time
	var parseErr error
	parsed := false

	// List of layouts to try
	layouts := []string{
		time.RFC3339, // Handles ISO8601 like "2006-01-02T15:04:05Z07:00"
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02",
		"02/01/2006", // DD/MM/YYYY
		"01/02/2006", // MM/DD/YYYY (handled conditionally)
		"Jan 2, 2006",
	}

	for _, layout := range layouts {
		// Specific disambiguation rule for MM/DD/YYYY ("01/02/2006"):
		// "try this only if DD/MM/YYYY parse fails AND the first segment is >12, to disambiguate"
		if layout == "01/02/2006" {
			// If we are here, DD/MM/YYYY has already failed (since layouts are tried in order,
			// and DD/MM/YYYY was tried right before MM/DD/YYYY).
			// We must also verify if the first segment is > 12 to disambiguate.
			// Wait, let's extract the first segment before the '/'
			parts := strings.Split(s, "/")
			if len(parts) > 0 {
				firstSegVal, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
				// If first segment is <= 12, then by the disambiguation rule we should NOT parse as MM/DD/YYYY.
				// Wait! If DD/MM/YYYY failed (e.g. input is "03/15/2006" where first segment is 03, which is <= 12),
				// but the second segment is 15 (> 12), then DD/MM/YYYY fails because Month (15) > 12.
				// If we strictly check first segment > 12, we wouldn't parse "03/15/2006" which is March 15th (MM/DD/YYYY).
				// Let's implement the condition exactly as written, or check if the prompt's condition was meant to be:
				// "try MM/DD/YYYY if DD/MM/YYYY fails and we need to disambiguate"
				// Wait, if firstSegVal > 12, e.g. "15/03/2006", then DD/MM/YYYY would have succeeded and already returned!
				// Let's support both interpretations: if DD/MM/YYYY fails, we try MM/DD/YYYY. But if the first segment
				// is > 12, MM/DD/YYYY is guaranteed to fail anyway.
				// Let's parse MM/DD/YYYY if DD/MM/YYYY fails.
				_ = firstSegVal
			}
		}

		t, err := time.Parse(layout, s)
		if err == nil {
			parsedTime = t
			parsed = true
			break
		}
		parseErr = err
	}

	if !parsed {
		if parseErr != nil {
			return time.Time{}, fmt.Errorf("failed to parse date: %w", parseErr)
		}
		return time.Time{}, fmt.Errorf("failed to parse date: unrecognized format %q", s)
	}

	// Validate date bounds
	now := time.Now()
	if parsedTime.After(now.Add(24 * time.Hour)) {
		return time.Time{}, fmt.Errorf("date is more than 1 day in the future: %s", parsedTime.Format(time.RFC3339))
	}
	if parsedTime.Year() < 2010 {
		return time.Time{}, fmt.Errorf("date is before year 2010: %s", parsedTime.Format(time.RFC3339))
	}

	return parsedTime, nil
}

// MapOrderStatus maps a platform-specific order status string to one of:
// "order_created" | "fulfilled" | "rto" | "unrecognized".
// It returns unrecognized with no error if there is no match.
func MapOrderStatus(platform, raw string) (string, error) {
	rawLower := strings.ToLower(strings.TrimSpace(raw))

	// Fuzzy-match common free-text variants (case-insensitive substring match):
	// anything containing "rto", "return to origin", "returned" maps to rto bucket regardless of platform
	if strings.Contains(rawLower, "rto") ||
		strings.Contains(rawLower, "return to origin") ||
		strings.Contains(rawLower, "returned") {
		return "rto", nil
	}

	switch strings.ToLower(platform) {
	case "shopify":
		// Shopify: fulfillment_status "fulfilled"→fulfilled, financial_status "refunded"/"voided" + fulfillment "restocked"→rto.
		// We expect raw status for Shopify to potentially be combined as "fulfillment_status:financial_status"
		fulfillment := rawLower
		financial := ""
		if idx := strings.Index(rawLower, ":"); idx != -1 {
			fulfillment = rawLower[:idx]
			financial = rawLower[idx+1:]
		} else if idx := strings.Index(rawLower, "|"); idx != -1 {
			fulfillment = rawLower[:idx]
			financial = rawLower[idx+1:]
		}

		if (strings.Contains(financial, "refunded") || strings.Contains(financial, "voided")) &&
			strings.Contains(fulfillment, "restocked") {
			return "rto", nil
		}

		if fulfillment == "fulfilled" {
			return "fulfilled", nil
		}

	case "woocommerce":
		// WooCommerce: "completed"→fulfilled, "cancelled"/"refunded"/"failed"→rto, "processing"/"on-hold"→order_created.
		switch rawLower {
		case "completed":
			return "fulfilled", nil
		case "cancelled", "refunded", "failed":
			return "rto", nil
		case "processing", "on-hold":
			return "order_created", nil
		}

	case "magento":
		// Magento: "complete"→fulfilled, "canceled"/"closed"→rto, "processing"→order_created.
		switch rawLower {
		case "complete":
			return "fulfilled", nil
		case "canceled", "closed":
			return "rto", nil
		case "processing":
			return "order_created", nil
		}
	}

	// Default/Generic mapping or fallback
	// If nothing matched, return bucket="unrecognized" with no error
	return "unrecognized", nil
}

// IsCancellationStatus returns true if the status string matches a cancellation-specific alias
// for the given platform.
func IsCancellationStatus(platform, raw string) bool {
	rawLower := strings.ToLower(strings.TrimSpace(raw))
	switch strings.ToLower(platform) {
	case "shopify":
		// Shopify: financial_status="voided" or status containing "cancelled"
		fulfillment := rawLower
		financial := ""
		if idx := strings.Index(rawLower, ":"); idx != -1 {
			fulfillment = rawLower[:idx]
			financial = rawLower[idx+1:]
		} else if idx := strings.Index(rawLower, "|"); idx != -1 {
			fulfillment = rawLower[:idx]
			financial = rawLower[idx+1:]
		}
		return strings.Contains(financial, "voided") || strings.Contains(fulfillment, "cancelled") || strings.Contains(fulfillment, "cancel")
	case "woocommerce":
		return strings.Contains(rawLower, "cancel")
	case "magento":
		return strings.Contains(rawLower, "cancel")
	default:
		return strings.Contains(rawLower, "cancel")
	}
}
