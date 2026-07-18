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
	ID                         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	PhoneNormalized            string     `gorm:"uniqueIndex;not null" json:"phone_normalized"`
	NetworkTotalOrders         int        `gorm:"not null;default:0" json:"network_total_orders"`
	NetworkRTOCount            int        `gorm:"not null;default:0" json:"network_rto_count"`
	LastUpdatedAt              time.Time  `gorm:"not null" json:"last_updated_at"`

	// LTV computation fields — populated by the buyer profile aggregation job
	FirstNetworkOrderAt        *time.Time `gorm:"default:null" json:"first_network_order_at"`
	LastNetworkOrderAt         *time.Time `gorm:"default:null" json:"last_network_order_at"`
	NetworkTotalSpendINR       float64    `gorm:"type:numeric(14,2);default:0" json:"network_total_spend_inr"`
	NetworkAvgOrderValueINR    float64    `gorm:"type:numeric(14,2);default:0" json:"network_avg_order_value_inr"`
	NetworkOrderValueStdDevINR float64    `gorm:"type:numeric(14,2);default:0" json:"network_order_value_std_dev_inr"`
}

// MonthsSinceFirstNetworkOrder returns the number of months between
// the buyer's first network order and now. Used for LTV projection.
func (bp *BuyerProfile) MonthsSinceFirstNetworkOrder() float64 {
	if bp.FirstNetworkOrderAt == nil || bp.FirstNetworkOrderAt.IsZero() {
		return 0
	}
	return time.Since(*bp.FirstNetworkOrderAt).Hours() / (24 * 30)
}

// DaysSinceLastNetworkOrder returns the number of days since
// the buyer's most recent order across the network.
func (bp *BuyerProfile) DaysSinceLastNetworkOrder() int {
	if bp.LastNetworkOrderAt == nil || bp.LastNetworkOrderAt.IsZero() {
		return 999
	}
	return int(time.Since(*bp.LastNetworkOrderAt).Hours() / 24)
}

// OrderValueConsistencyScore returns a 0-1 score representing how
// consistent this buyer's order values are across the network.
// High consistency = more predictable LTV = higher confidence score.
func (bp *BuyerProfile) OrderValueConsistencyScore() float64 {
	if bp.NetworkOrderValueStdDevINR == 0 || bp.NetworkAvgOrderValueINR == 0 {
		return 0.5 // neutral when we don't have std dev data
	}
	// Coefficient of variation: lower = more consistent
	cv := bp.NetworkOrderValueStdDevINR / bp.NetworkAvgOrderValueINR
	// Convert to 0-1 score where 1 = perfectly consistent
	if cv >= 1.0 {
		return 0.0
	}
	return 1.0 - cv
}
