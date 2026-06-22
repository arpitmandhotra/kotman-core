package domain

import (
	"time"
	"gorm.io/gorm"
)

// TrustRequest represents the payload we expect from Shopify.
// WHY IT'S NECESSARY: Go is strictly typed. It needs a blueprint to map raw JSON text into memory.
type TrustRequest struct {
	// IMPORTANT: Notice how 'PhoneHash' starts with a Capital Letter.
	// In Go, Capitalized fields are "Public" (exported). Lowercase are "Private", we need to do that so that fiber in other package can map these out else the functionality will break.
	// If you make this 'phoneHash', the JSON parser cannot see it and it will fail silently.
	PhoneHash string `json:"phone_hash"`

	// The `json:"..."` part is a Struct Tag.
	// WHY IT'S NECESSARY: It tells Go's JSON parser, "When you see 'session_id' in the raw JSON, put that value into this SessionID field."
	SessionID string `json:"session_id"`
}

// TrustResponse is the blueprint for the JSON we send back.
type TrustResponse struct {
	PhoneHash string `json:"phone_hash"`
	Score     int    `json:"score"`
	Action    string `json:"action"`
}

type WebhookPayload struct {
	//The variables here need to have there name started from capital letters so that the fiber mapping the json can see it 
	Phone  string `json:"phone"`
	Reason string `json:"reason"`
}

type TrustProfile struct {
	gorm.Model // This automatically gives us ID, CreatedAt, UpdatedAt, and DeletedAt

	PhoneHash           string `gorm:"uniqueIndex;not null"`
	FirstSeenMerchantID string `gorm:"index"`

	// --- IMMUTABLE HISTORICAL FACTS (The AI's Diet) ---
	TotalOrders          int     `gorm:"default:0"`
	SuccessfulDeliveries int     `gorm:"default:0"`
	TotalRTOs            int     `gorm:"default:0"`
	TotalCancellations   int     `gorm:"default:0"`
	TotalRevenueSpent    float64 `gorm:"default:0"`
	LastActivityDate     *time.Time

	// --- System Overrides ---
	IsBlacklisted   bool       `gorm:"default:false"`
	BlacklistReason string     `json:"reason"`
	LockedAt        *time.Time
}

// GenerateAIFeatures takes stored historical facts and computes live metrics for the ML model
func (p *TrustProfile) GenerateAIFeatures(currentOrderValue float64) map[string]interface{} {
	rtoRate := 0.0
	if p.TotalOrders > 0 {
		rtoRate = float64(p.TotalRTOs) / float64(p.TotalOrders)
	}

	cancellationFreq := 0.0
	if p.TotalOrders > 0 {
		cancellationFreq = float64(p.TotalCancellations) / float64(p.TotalOrders)
	}

	avgOrderValue := 0.0
	if p.SuccessfulDeliveries > 0 {
		avgOrderValue = p.TotalRevenueSpent / float64(p.SuccessfulDeliveries)
	}

	orderValueRatio := 1.0
	if avgOrderValue > 0 {
		orderValueRatio = currentOrderValue / avgOrderValue
	}

	// This is the exact data contract payload your friend will fire to their Python AI URL
	return map[string]interface{}{
		"network_rto_rate":             rtoRate,
		"cancellation_frequency":       cancellationFreq,
		"order_value_to_average_ratio": orderValueRatio,
		"total_orders_network":         p.TotalOrders,
		"account_age_days":             time.Since(p.CreatedAt).Hours() / 24,
	}
}