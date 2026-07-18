package service

import (
	"context"
	"math"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
)

// LTVService computes predicted 90-day lifetime value for buyers
// using their cross-network purchase history stored in BuyerProfile.
type LTVService struct {
	pg *gorm.DB
}

// NewLTVService creates a new LTVService.
func NewLTVService(pg *gorm.DB) *LTVService {
	return &LTVService{pg: pg}
}

// ComputePredictedLTV calculates a buyer's predicted 90-day LTV
// from their cross-network purchase history.
//
// Algorithm:
// 1. Fetch buyer's network-wide order history from buyer_profiles
// 2. Calculate average monthly spend and order frequency
// 3. Project 90-day spend assuming continuation of observed pattern
// 4. Apply a recency decay — more recent orders weighted more heavily
// 5. Compute a confidence score based on data depth and recency
//
// Returns a PredictedLTVSignal with raw fallback if data is insufficient.
func (s *LTVService) ComputePredictedLTV(
	ctx context.Context,
	buyerPhone string,
	rawTransactionValueINR float64,
) (*domain.PredictedLTVSignal, error) {
	signal := &domain.PredictedLTVSignal{
		BuyerPhoneNormalized:   buyerPhone,
		RawTransactionValueINR: rawTransactionValueINR,
		PredictionMethod:       "raw_fallback",
	}

	// Fetch buyer profile from cross-network store
	var profile domain.BuyerProfile
	if err := s.pg.WithContext(ctx).Where("phone_normalized = ?", buyerPhone).First(&profile).Error; err != nil {
		// No cross-network data — return raw fallback
		signal.FallbackReason = "no_buyer_profile"
		return signal, nil
	}

	signal.NetworkOrderCount = profile.NetworkTotalOrders
	signal.NetworkTotalSpendINR = profile.NetworkTotalSpendINR
	signal.NetworkAvgOrderValueINR = profile.NetworkAvgOrderValueINR

	// Require minimum 3 orders before computing LTV
	if profile.NetworkTotalOrders < 3 {
		signal.FallbackReason = "insufficient_orders"
		return signal, nil
	}

	// Compute average monthly spend from network history
	// Use the period from first to last order to compute rate
	monthsSinceFirstOrder := profile.MonthsSinceFirstNetworkOrder()
	if monthsSinceFirstOrder < 0.5 {
		// Less than ~2 weeks of history — too early to predict
		signal.FallbackReason = "insufficient_history_window"
		return signal, nil
	}

	avgMonthlySpend := profile.NetworkTotalSpendINR / monthsSinceFirstOrder

	// Project 90-day (3-month) LTV
	raw90DayLTV := avgMonthlySpend * 3.0

	// Apply recency decay — if last order was more than 90 days ago,
	// reduce confidence and LTV projection proportionally
	daysSinceLastOrder := profile.DaysSinceLastNetworkOrder()
	recencyMultiplier := 1.0
	if daysSinceLastOrder > 90 {
		// Buyer may be churned — decay the projection
		decayFactor := math.Exp(-0.01 * float64(daysSinceLastOrder-90))
		recencyMultiplier = math.Max(0.3, decayFactor)
	}

	signal.Predicted90DayLTVINR = raw90DayLTV * recencyMultiplier

	// Compute confidence score based on:
	// - Number of orders (more = higher confidence)
	// - Recency (recent = higher confidence)
	// - Consistency (low variance in order amounts = higher confidence)
	orderCountScore := math.Min(1.0, float64(profile.NetworkTotalOrders)/10.0)
	recencyScore := math.Max(0.0, 1.0-(float64(daysSinceLastOrder)/180.0))
	consistencyScore := profile.OrderValueConsistencyScore() // 0-1, computed from buyer profile

	signal.ConfidenceScore = (orderCountScore*0.4 + recencyScore*0.4 + consistencyScore*0.2)
	signal.PredictionMethod = "network_history"

	return signal, nil
}
