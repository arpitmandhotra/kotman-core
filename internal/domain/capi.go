package domain

import "time"

// PredictedLTVSignal represents a buyer's predicted 90-day lifetime value
// computed from cross-network purchase history.
//
// This is transmitted to Meta CAPI as the "value" field in compliance with
// Meta's documented support for LTV-based value optimization:
// https://developers.facebook.com/docs/marketing-api/conversions-api/best-practices
//
// Meta explicitly supports passing predicted LTV instead of raw transaction
// value for advertisers with reliable LTV models. The confidence threshold
// and minimum data point requirements ensure we only send predictions
// grounded in sufficient evidence.
type PredictedLTVSignal struct {
	BuyerPhoneNormalized string
	MerchantID           string

	// The actual transaction that triggered this signal
	RawTransactionValueINR float64

	// Cross-network data used to compute the prediction
	NetworkOrderCount       int     // total orders across all Kaughtman merchants
	NetworkTotalSpendINR    float64 // total confirmed spend across the network
	NetworkAvgOrderValueINR float64
	NetworkOrderFrequency   float64 // avg orders per 30 days across network

	// The prediction
	Predicted90DayLTVINR float64 // what we send to Meta as "value"
	ConfidenceScore      float64 // 0.0 to 1.0
	PredictionMethod     string  // "network_history" | "raw_fallback"

	// Audit trail — stored for every signal sent
	SentToMeta     bool
	SentValueINR   float64   // the actual value sent (may differ from prediction)
	SentAt         time.Time
	FallbackReason string // populated when we fall back to raw value
}

// MetaCAPIValue returns the value to send to Meta.
// This is the ONLY function that should determine what value goes to Meta.
// It enforces the confidence threshold that makes our LTV signals defensible.
func (s *PredictedLTVSignal) MetaCAPIValue() (value float64, method string) {
	// HARD REQUIREMENT: minimum 3 cross-network orders before we send LTV
	// Below this threshold we have insufficient data to predict LTV reliably
	if s.NetworkOrderCount < 3 {
		return s.RawTransactionValueINR, "raw_fallback:insufficient_orders"
	}

	// HARD REQUIREMENT: confidence score must be at least 0.65
	// Below this threshold our prediction is not reliable enough to send
	if s.ConfidenceScore < 0.65 {
		return s.RawTransactionValueINR, "raw_fallback:low_confidence"
	}

	// HARD REQUIREMENT: predicted LTV must be at least equal to raw transaction
	// We never send a value LOWER than the actual transaction (that would hurt ROAS)
	// We also cap the multiplier at 5x to prevent outlier distortion
	if s.Predicted90DayLTVINR < s.RawTransactionValueINR {
		return s.RawTransactionValueINR, "raw_fallback:ltv_below_transaction"
	}
	maxLTV := s.RawTransactionValueINR * 5.0
	if s.Predicted90DayLTVINR > maxLTV {
		return maxLTV, "network_history:capped_5x"
	}

	return s.Predicted90DayLTVINR, "network_history"
}

// IsLTVPrediction returns true if we are sending a predicted LTV
// rather than the raw transaction value. Used for analytics and audit.
func (s *PredictedLTVSignal) IsLTVPrediction() bool {
	_, method := s.MetaCAPIValue()
	return method == "network_history" || method == "network_history:capped_5x"
}

// CAPIEventLog stores an immutable audit record for every CAPI event
// dispatched to Meta. This is the defence record if Meta ever questions
// a merchant's conversion data.
type CAPIEventLog struct {
	ID             string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID     string `gorm:"index;not null"`
	OrderID        string `gorm:"index;not null"`
	BuyerPhoneHash string `gorm:"not null"` // SHA-256 of normalized phone (Meta format)

	// What we sent to Meta
	SentValueINR      float64 `gorm:"not null"`
	RawTransactionINR float64 `gorm:"not null"`
	PredictionMethod  string  `gorm:"not null"` // "network_history" | "raw_fallback:..."
	ConfidenceScore   float64
	NetworkOrderCount int

	// Meta's response
	MetaEventID      string
	MetaFBTraceID    string
	MetaResponseCode int
	MetaErrorMessage string

	// Timestamps
	SentAt    time.Time `gorm:"index"`
	CreatedAt time.Time
}

// CAPILTVCoverage provides coverage statistics for the merchant's CAPI
// LTV signal quality. Exposed in the insights API response so merchants
// can see how much of their buyer base has LTV predictions vs raw fallback.
type CAPILTVCoverage struct {
	// How many unique buyers in this merchant's base have LTV predictions
	LTVBuyerCount int `json:"capi_ltv_buyer_count"`
	// How many fall back to raw transaction value
	RawBuyerCount int `json:"capi_raw_buyer_count"`
	// Percentage with LTV predictions
	LTVCoveragePct float64 `json:"capi_ltv_coverage_pct"`
	// Total CAPI events sent this month
	EventsSentMonth int `json:"capi_events_sent_month"`
	// Average multiplier effect (avg sent value / avg raw value)
	AvgLTVMultiplier float64 `json:"capi_avg_ltv_multiplier"`
}
