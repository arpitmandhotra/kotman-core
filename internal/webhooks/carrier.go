package webhooks

import (
	"context"
	"net/http"
	"time"
)

// NormalizedNDREvent represents a unified delivery exception report across all courier partners.
type NormalizedNDREvent struct {
	TrackingAWB     string    `json:"tracking_awb" db:"tracking_awb"`
	CarrierName     string    `json:"carrier_name" db:"carrier_name"`
	InternalOrderID string    `json:"internal_order_id" db:"internal_order_id"`
	StatusCode      string    `json:"status_code" db:"status_code"`
	NDRReason       string    `json:"ndr_reason" db:"ndr_reason"`
	AttemptCount    int       `json:"attempt_count" db:"attempt_count"`
	EventTimestamp  time.Time `json:"event_timestamp" db:"event_timestamp"`
}

// CarrierWebhookHandler dictates the interface every logistics integration strategy must implement.
type CarrierWebhookHandler interface {
	// ValidateSignature verifies the authenticity of the webhook sender.
	ValidateSignature(ctx context.Context, req *http.Request, body []byte, secret []byte) error

	// ParsePayload parses the raw JSON request body into our unified event schema.
	ParsePayload(ctx context.Context, body []byte) (*NormalizedNDREvent, error)

	// GetCarrierName returns the system name of the logistics partner (e.g. "delhivery", "shiprocket").
	GetCarrierName() string
}
