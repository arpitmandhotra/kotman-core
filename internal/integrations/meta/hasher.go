package meta

import (
    "crypto/sha256"
    "encoding/hex"
    "strings"
    "unicode"
)

// HashForMeta produces a plain SHA-256 hex digest suitable for Meta's
// Conversions API user_data fields. This is intentionally different from
// Kotman's internal HashPhone (which uses peppered HMAC-SHA256).
// Meta requires plain SHA-256 to match against their identity graph.
// Input is lowercased and whitespace-stripped before hashing per Meta spec.
func HashForMeta(raw string) string {
    normalized := strings.ToLower(
        strings.Map(func(r rune) rune {
            if unicode.IsSpace(r) { return -1 }
            return r
        }, raw),
    )
    if normalized == "" { return "" }
    h := sha256.Sum256([]byte(normalized))
    return hex.EncodeToString(h[:])
}

// HashPhoneForMeta normalizes an Indian phone number to E.164 format
// (+91XXXXXXXXXX) then hashes it for Meta. Applies the same 4-format
// normalization logic from internal/crypto/phone.go but WITHOUT the pepper.
// Must stay in sync with normalisePhone in crypto/phone.go.
func HashPhoneForMeta(rawPhone string) string {
    // strip spaces and dashes
    clean := strings.Map(func(r rune) rune {
        if unicode.IsSpace(r) || r == '-' { return -1 }
        return r
    }, rawPhone)

    var normalized string
    switch {
    case len(clean) == 10:
        normalized = "+91" + clean
    case len(clean) == 11 && clean[0] == '0':
        normalized = "+91" + clean[1:]
    case len(clean) == 12 && strings.HasPrefix(clean, "91"):
        normalized = "+" + clean
    case len(clean) == 13 && strings.HasPrefix(clean, "091"):
        normalized = "+" + clean[1:]
    case len(clean) == 14 && strings.HasPrefix(clean, "0091"):
        normalized = "+" + clean[2:]
    default:
        normalized = clean
    }
    return HashForMeta(normalized)
}
