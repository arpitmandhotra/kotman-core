package backfill

import (
	"regexp"
	"strings"
)

// validateIndianMobilePhone returns the cleaned 10-digit mobile number and true
// if the input is a plausible Indian mobile number, or ("", false) if it should
// be rejected. It does not call HashPhone — that remains the caller's responsibility.
//
// Validation rules (in order):
//  1. Strip all non-digit characters.
//  2. Normalise country-code prefixes (+91, 91, 0).
//  3. Reject if not exactly 10 digits after normalisation.
//  4. Reject if first digit < '6' (landlines, not a mobile prefix).
//  5. Reject all-same-digit numbers (9999999999, 8888888888, etc.).
//  6. Reject a curated list of known placeholder numbers.
func validateIndianMobilePhone(raw string) (string, bool) {
	// Strip all non-digit characters: spaces, dashes, dots, parentheses, +
	digitsOnly := regexp.MustCompile(`\D`).ReplaceAllString(raw, "")

	// Handle country code prefix: +91 or 91 prefix on a 12-digit string
	if len(digitsOnly) == 12 && strings.HasPrefix(digitsOnly, "91") {
		digitsOnly = digitsOnly[2:]
	}
	// Handle leading 0 (STD trunk prefix): 09876012345 → 9876012345
	if len(digitsOnly) == 11 && strings.HasPrefix(digitsOnly, "0") {
		digitsOnly = digitsOnly[1:]
	}

	// Must be exactly 10 digits after normalisation
	if len(digitsOnly) != 10 {
		return "", false
	}

	// Indian mobile numbers start with 6, 7, 8, or 9.
	// Landlines start with 0–5 — reject those.
	firstDigit := digitsOnly[0]
	if firstDigit < '6' {
		return "", false
	}

	// Reject known placeholder / garbage patterns.
	// A real mobile number is never all the same digit.
	allSame := true
	for _, ch := range digitsOnly[1:] {
		if byte(ch) != digitsOnly[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return "", false // catches 9999999999, 8888888888, 0000000000, etc.
	}

	// Reject specific known garbage numbers used as placeholders.
	// Do NOT expand this list without a real data sample — premature expansion
	// causes false rejections of legitimate customers.
	knownGarbage := map[string]bool{
		"1234567890": true,
		"0123456789": true,
		"9876543210": true,
		"1111111111": true,
		"1234512345": true,
		"9999900000": true,
		"9000000000": true,
		"8000000000": true,
		"7000000000": true,
		"6000000000": true,
	}
	if knownGarbage[digitsOnly] {
		return "", false
	}

	return digitsOnly, true
}
