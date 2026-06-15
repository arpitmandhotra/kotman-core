package crypto

import (
    "crypto/sha256"
    "fmt"
    "strings"
    "unicode"
)

// HashPhone takes a raw phone number in any format,
// normalises it to digits only with country code,
// and returns a lowercase hex SHA-256 hash.
//
// Examples of inputs that all produce the same hash:
//   +91 98765 43210
//   +919876543210
//   09876543210
//   9876543210
func HashPhone(raw string) string {
    normalised := normalisePhone(raw)
    hash := sha256.Sum256([]byte(normalised))
    return fmt.Sprintf("%x", hash)
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
    // 10 digits  → add 91 prefix  (e.g. 9876543210)
    // 11 digits starting with 0 → strip 0, add 91 (e.g. 09876543210)
    // 12 digits starting with 91 → already canonical (e.g. 919876543210)
    // 13 digits starting with 091 → strip leading 0 (e.g. 0919876543210)
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