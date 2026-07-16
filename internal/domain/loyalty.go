package domain

import (
	"time"

	"github.com/google/uuid"
)

type BuyerLoyaltySnapshot struct {
	ID                              uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	MerchantID                      uuid.UUID `gorm:"type:uuid;not null;index" json:"merchant_id"`
	ComputedAt                      time.Time `gorm:"not null" json:"computed_at"`
	PeriodStartAt                   time.Time `gorm:"not null" json:"period_start_at"` // rolling 30-day window start
	PeriodEndAt                     time.Time `gorm:"not null" json:"period_end_at"`   // rolling 30-day window end

	// Metric 1 — Repeat Rate (free tier, store-level)
	TotalUniqueBuyers               int       `gorm:"not null;default:0" json:"total_unique_buyers"`
	RepeatBuyers                    int       `gorm:"not null;default:0" json:"repeat_buyers"`                      // unique phones with 2+ orders on THIS merchant
	RepeatRatePct                   float64   `gorm:"not null;default:0" json:"repeat_rate_pct"`                    // (RepeatBuyers / TotalUniqueBuyers) * 100
	PrevRepeatRatePct               float64   `gorm:"not null;default:0" json:"prev_repeat_rate_pct"`               // for trend calculation
	RepeatRateTrendPct              float64   `gorm:"not null;default:0" json:"repeat_rate_trend_pct"`              // RepeatRatePct - PrevRepeatRatePct

	// Metric 2 — True Repeat Rate (growth tier, cross-network)
	TrueRepeatBuyers                int       `gorm:"not null;default:0" json:"true_repeat_buyers"`                 // repeat buyers with zero RTOs network-wide
	TrueRepeatRatePct               float64   `gorm:"not null;default:0" json:"true_repeat_rate_pct"`               // (TrueRepeatBuyers / TotalUniqueBuyers) * 100
	PrevTrueRepeatRatePct           float64   `gorm:"not null;default:0" json:"prev_true_repeat_rate_pct"`
	TrueRepeatRateTrendPct          float64   `gorm:"not null;default:0" json:"true_repeat_rate_trend_pct"`
	ShopifyEquivalentRepeatRatePct *float64   `json:"shopify_equivalent_repeat_rate_pct"` // Metric 2 email equivalent

	// Metric 3 — Repeat RTO Abusers (growth tier, cross-network)
	RepeatRTOAbuserCount            int       `gorm:"not null;default:0" json:"repeat_rto_abuser_count"`            // unique phones: 3+ orders AND network RTO rate > 40%
	RepeatRTOAbuserTotalRTOs        int       `gorm:"not null;default:0" json:"repeat_rto_abuser_total_rtos"`        // sum of RTO orders for these buyers on THIS merchant
	RepeatRTOAbuserEstimatedCostINR int       `gorm:"not null;default:0" json:"repeat_rto_abuser_estimated_cost_inr"` // RepeatRTOAbuserTotalRTOs * 210 (rupees)

	// Minimum data threshold
	HasSufficientData               bool      `gorm:"not null;default:false" json:"has_sufficient_data"` // false if < 50 unique buyers or < 30 days data
	InsufficientDataReason          string    `gorm:"default:''" json:"insufficient_data_reason"`        // "insufficient_buyers" | "insufficient_history" | ""
}

type BuyerLoyaltyInsights struct {
	// Always returned (free tier)
	RepeatRatePct      float64 `json:"repeat_rate_pct"`
	RepeatRateTrendPct float64 `json:"repeat_rate_trend_pct"`

	// Growth tier only — nil for free tier
	TrueRepeatRatePct              *float64 `json:"true_repeat_rate_pct"`
	TrueRepeatRateTrendPct         *float64 `json:"true_repeat_rate_trend_pct"`
	ShopifyEquivalentRepeatRatePct *float64 `json:"shopify_equivalent_repeat_rate_pct"`
	RepeatRTOAbuserCount           *int     `json:"repeat_rto_abuser_count"`
	RepeatRTOAbuserTotalRTOs       *int     `json:"repeat_rto_abuser_total_rtos"`
	RepeatRTOAbuserEstimatedCostINR *int     `json:"repeat_rto_abuser_estimated_cost_inr"`

	// Data availability
	HasSufficientData      bool   `json:"has_sufficient_data"`
	InsufficientDataReason string `json:"insufficient_data_reason"`
}

type BuyerProfile struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	PhoneNormalized    string    `gorm:"uniqueIndex;not null" json:"phone_normalized"`
	NetworkTotalOrders int       `gorm:"not null;default:0" json:"network_total_orders"`
	NetworkRTOCount    int       `gorm:"not null;default:0" json:"network_rto_count"`
	LastUpdatedAt      time.Time `gorm:"not null" json:"last_updated_at"`
}
