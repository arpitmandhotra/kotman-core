package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unicode"
)

var (
	pepper     string
	pepperOnce sync.Once
)

func loadPepper() {
	pepper = os.Getenv("HASH_PEPPER")
	if pepper == "" {
		slog.Error("SECURITY: HASH_PEPPER environment variable is missing — phone hashes are reversible without a pepper")
	}
}

// HashPhone normalises the phone and generates a true HMAC-SHA256 signature
func HashPhone(raw string) string {
	normalised := normalisePhone(raw)

	if len(normalised) < 10 {
		slog.Warn("phone number too short after normalisation — possible garbage input",
			"raw_length", len(raw),
			"normalised_length", len(normalised),
		)
	}

	pepperOnce.Do(loadPepper)

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
	case len(digits) == 14 && strings.HasPrefix(digits, "0091"):
		digits = digits[2:]
	case len(digits) == 13 && strings.HasPrefix(digits, "091"):
		digits = digits[1:]
	}

	// canonical form: 919876543210
	return digits
}