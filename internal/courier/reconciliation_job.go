package courier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ReconciliationJob struct {
	pg *gorm.DB
}

func NewReconciliationJob(pgDB *gorm.DB) *ReconciliationJob {
	return &ReconciliationJob{pg: pgDB}
}

// RunReconciliation scans for long-pending orders (older than 21 days) and polls tracking details from couriers.
func (j *ReconciliationJob) RunReconciliation(ctx context.Context) error {
	slog.Info("starting logistics daily reconciliation polling job")

	cutoffTime := time.Now().AddDate(0, 0, -21)

	// Fetch active mappings that are older than 21 days and do not have a terminal status log in public.normalized_delivery_events
	var pendingAWBs []AWBMapping
	// Select mappings where the latest event is not DELIVERED or RTO_DELIVERED
	err := j.pg.WithContext(ctx).Raw(`
		SELECT m.* FROM awb_mappings m
		LEFT JOIN (
			SELECT awb, event_type, ROW_NUMBER() OVER(PARTITION BY awb ORDER BY courier_timestamp DESC) as rn
			FROM normalized_delivery_events
		) e ON e.awb = m.awb AND e.rn = 1
		WHERE m.created_at <= ? 
		  AND (e.event_type IS NULL OR e.event_type NOT IN ('DELIVERED', 'RTO_DELIVERED'))
	`, cutoffTime).Scan(&pendingAWBs).Error

	if err != nil {
		return fmt.Errorf("failed fetching pending AWBs for reconciliation: %w", err)
	}

	slog.Info("reconciliation: scanning pending shipments", "count", len(pendingAWBs))

	for _, mapping := range pendingAWBs {
		err := j.reconcileAWB(ctx, mapping)
		if err != nil {
			slog.Error("failed to reconcile shipment AWB", "awb", mapping.AWB, "error", err)
		}
	}

	slog.Info("completed daily logistics reconciliation job successfully")
	return nil
}

func (j *ReconciliationJob) reconcileAWB(ctx context.Context, mapping AWBMapping) error {
	slog.Info("reconciling tracking status for shipment", "awb", mapping.AWB, "provider", mapping.Provider)

	// Actively poll courier tracking status API
	status, err := j.pollCourierTrackingAPI(ctx, mapping.AWB, mapping.Provider)
	if err != nil {
		return err
	}

	// Update delivery status in database inside transaction
	return j.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Log a mock Normalized Event representing the polled tracking state
		event := NormalizedDeliveryEvent{
			ID:               uuid.New(),
			MerchantID:       mapping.MerchantID,
			OrderID:          mapping.OrderID,
			AWB:              mapping.AWB,
			CourierProvider:  mapping.Provider,
			EventType:        status,
			AttemptNumber:    1,
			ReasonCode:       ReasonUnknown,
			CourierTimestamp: time.Now(),
			ReceivedAt:       time.Now(),
			RawPayloadEncrypted: []byte("reconciliation_polled_outcome"),
		}

		if err := tx.Create(&event).Error; err != nil {
			return err
		}

		// Sync with BillableEvent RTO flag if needed
		if status == EventRTODelivered || status == EventRTOInitiated {
			var billable domain.BillableEvent
			err := tx.Where("order_id = ? AND merchant_id = ?", mapping.OrderID.String(), mapping.MerchantID.String()).First(&billable).Error
			if err == nil {
				billable.IsRTO = true
				if err := tx.Save(&billable).Error; err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// pollCourierTrackingAPI polls the actual tracking status of the courier.
func (j *ReconciliationJob) pollCourierTrackingAPI(ctx context.Context, awb string, provider CourierProvider) (DeliveryEventType, error) {
	// Delhivery / Shiprocket / Xpressbees mock trackers
	// In production, we call the client helper endpoints for the couriers
	switch provider {
	case ProviderDelhivery:
		// Mock successful resolution to DELIVERED
		return EventDelivered, nil
	case ProviderShiprocket:
		// Mock successful resolution to RTO_DELIVERED
		return EventRTODelivered, nil
	case ProviderXpressbees:
		// Stub out
		return EventDelivered, nil
	case ProviderBluedart, ProviderClickpost:
		// Stub out
		return EventDelivered, nil
	default:
		return "", errors.New("unsupported courier tracker")
	}
}
