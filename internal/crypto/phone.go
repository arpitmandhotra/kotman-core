package crypto

import (
	"crypto/hmac"    // <-- We import the official hmac package
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"unicode"
)

// HashPhone normalises the phone and generates a true HMAC-SHA256 signature
func HashPhone(raw string) string {
	normalised := normalisePhone(raw)

	pepper := os.Getenv("HASH_PEPPER")
	if pepper == "" {
		slog.Warn("HASH_PEPPER environment variable is missing!")
	}

	// 1. Initialize the HMAC using SHA-256 and your secret pepper
	mac := hmac.New(sha256.New, []byte(pepper))
	
	// 2. Write the phone number into the HMAC generator
	mac.Write([]byte(normalised))
	
	// 3. Extract the final hash and convert it to a hex string
	return hex.EncodeToString(mac.Sum(nil))
}



// normalisePhone strips all non-digit characters,
// then ensures a canonical +91 prefix for Indian numbers.
func normalisePhone(raw string) string {
	// Step 1: strip everything that isn't a digit
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, raw)

	// Step 2: handle Indian number formats
	switch {
	case len(digits) == 10:
		digits = "91" + digits
	case len(digits) == 11 && strings.HasPrefix(digits, "0"):
		digits = "91" + digits[1:]
	case len(digits) == 13 && strings.HasPrefix(digits, "091"):
		digits = digits[1:]
	}

	// canonical form: 919876543210
	return digits
}