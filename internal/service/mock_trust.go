package service

import (
	"context"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// MockTrustService is a fake brain used only for local testing without a database.
type MockTrustService struct{}

func NewMockTrustService() *MockTrustService {
	return &MockTrustService{}
}

// EvaluateRisk strictly satisfies the TrustService interface (including the new IP, merchantID, and cartValue parameters)
func (s *MockTrustService) EvaluateRisk(ctx context.Context, phoneHash string, ipAddress string, merchantID string, cartValue float64) (domain.TrustResponse, error) {
	// The mock brain always returns a safe score of 85
	return domain.TrustResponse{
		PhoneHash:       phoneHash,
		BuyerTrustIndex: 85,
		Action:          "ALLOW_COD",
	}, nil
}

// ReportBadActor satisfies the interface but does nothing in the mock
func (s *MockTrustService) ReportBadActor(ctx context.Context, phoneHash string, reason string) error {
	return nil
}
