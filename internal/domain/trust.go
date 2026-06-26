package domain

import (
	"time"
	"gorm.io/gorm"
)

// TrustRequest represents the payload we expect from the frontend extension/Shopify.
type TrustRequest struct {
	// FIX: Replaced PhoneHash with Phone per architectural review
	Phone     string `json:"phone"`
	IPAddress string `json:"ip_address"`
	SessionID string `json:"session_id"`
}

// TrustResponse is the blueprint for the JSON we send back.
type TrustResponse struct {
	PhoneHash string `json:"phone_hash"`
	Score     int    `json:"score"`
	Action    string `json:"action"`

	// --- Dynamic Risk Pricing ---
	DeliveryFee     int  `json:"delivery_fee"`     
	DiscountPercent int  `json:"discount_percent"` 
	IsVIP           bool `json:"is_vip"`           
}

type WebhookPayload struct {
	Phone  string `json:"phone"`
	Reason string `json:"reason"`
}

type TrustProfile struct {
	gorm.Model 

	PhoneHash           string `gorm:"uniqueIndex;not null"`
	FirstSeenMerchantID string `gorm:"index"`

	// --- IMMUTABLE HISTORICAL FACTS (The AI's Diet) ---
	TotalOrders          int     `gorm:"default:0"`
	SuccessfulDeliveries int     `gorm:"default:0"`
	TotalRTOs            int     `gorm:"default:0"`
	TotalCancellations   int     `gorm:"default:0"`
	TotalRevenueSpent    float64 `gorm:"default:0"`
	LastActivityDate     *time.Time

	// --- INTENT-WEIGHTED FEEDBACK ---
	ComplaintCount   int        `gorm:"default:0"`
	ComplaintScore   float64    `gorm:"default:0"` // Cumulative running sentiment score
	LastComplaintAt  *time.Time
	RiskAdjustment   float64    `gorm:"default:0"` // Accumulator for feedback weights

	// --- System Overrides ---
	IsBlacklisted   bool       `gorm:"default:false"`
	BlacklistReason string     `json:"reason"`
	LockedAt        *time.Time
}

// CustomerFeedback stores individual ticket data linked to the merchant and profile
type CustomerFeedback struct {
	gorm.Model
	PhoneHash  string    `gorm:"index"`
	MerchantID string    `gorm:"index"`
	OrderID    string
	Category   string    
	Sentiment  float64   
	SKU        string    `gorm:"index"`
	ReceivedAt time.Time
}

// FeedbackWeight maps complaint categories to actual risk adjustments
type FeedbackWeight struct {
	BuyerRiskDelta float64
	MerchantSignal bool
	ProductSignal  bool
}

// FeedbackWeights defines the severity of different post-purchase events
var FeedbackWeights = map[string]FeedbackWeight{
	"FRAUD_SUSPECTED":  {BuyerRiskDelta: -30.0, MerchantSignal: false, ProductSignal: false},
	"NOT_AS_DESCRIBED": {BuyerRiskDelta: -5.0,  MerchantSignal: true,  ProductSignal: true},
	"PRODUCT_DEFECT":   {BuyerRiskDelta: -2.0,  MerchantSignal: true,  ProductSignal: true},
	"SIZE_MISMATCH":    {BuyerRiskDelta: -2.0,  MerchantSignal: true,  ProductSignal: true},
	"DELIVERY_DAMAGE":  {BuyerRiskDelta: -1.0,  MerchantSignal: true,  ProductSignal: false},
	"CHANGED_MIND":     {BuyerRiskDelta: -0.5,  MerchantSignal: false, ProductSignal: false},
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

	// This is the exact data contract payload fired to the Python AI
	return map[string]interface{}{
		"network_rto_rate":             rtoRate,
		"cancellation_frequency":       cancellationFreq,
		"order_value_to_average_ratio": orderValueRatio,
		"total_orders_network":         p.TotalOrders,
		"account_age_days":             time.Since(p.CreatedAt).Hours() / 24,
		"risk_adjustment":              p.RiskAdjustment, // FIX: Injects the feedback math directly into the AI scorer
	}
}