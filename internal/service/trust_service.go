package service

import (
	"context"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

type TrustService interface {
	// NEW: Added ipAddress string to the signature
	EvaluateRisk(ctx context.Context, phoneHash string, ipAddress string, merchantID string, cartValue float64) (domain.TrustResponse, error)
	//A function which is saving the details about the scammer
	ReportBadActor(ctx context.Context, phoneHash string, reason string) error
}
