package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/security"
)

// ShiprocketHandler concrete strategy implementation.
type ShiprocketHandler struct{}

func NewShiprocketHandler() *ShiprocketHandler {
	return &ShiprocketHandler{}
}

func (h *ShiprocketHandler) GetCarrierName() string {
	return "shiprocket"
}

// ShiprocketRawPayload models the Shiprocket NDR webhook structure.
type ShiprocketRawPayload struct {
	AWBCode       string `json:"awb_code"`
	OrderID       string `json:"order_id"`
	CurrentStatus string `json:"current_status"` // e.g. "undelivered"
	NDR           struct {
		Reason       string    `json:"reason"`
		AttemptCount int       `json:"attempt_count"`
		NDRDate      string    `json:"ndr_date"` // formatted as "2026-07-12 14:15:00"
	} `json:"ndr"`
}

func (h *ShiprocketHandler) ValidateSignature(ctx context.Context, req *http.Request, body []byte, secret []byte) error {
	providedHmac := req.Header.Get("X-Shiprocket-Hmac-Sha256")
	if providedHmac == "" {
		return errors.New("missing X-Shiprocket-Hmac-Sha256 header")
	}

	if !security.ValidateHMAC(body, providedHmac, secret) {
		return errors.New("invalid shiprocket HMAC signature")
	}
	return nil
}

func (h *ShiprocketHandler) ParsePayload(ctx context.Context, body []byte) (*NormalizedNDREvent, error) {
	var payload ShiprocketRawPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal shiprocket payload: %w", err)
	}

	if payload.AWBCode == "" {
		return nil, errors.New("missing AWB code in shiprocket payload")
	}

	timestamp := time.Now()
	if payload.NDR.NDRDate != "" {
		parsedTime, err := time.Parse("2006-01-02 15:04:05", payload.NDR.NDRDate)
		if err == nil {
			timestamp = parsedTime
		}
	}

	return &NormalizedNDREvent{
		TrackingAWB:     payload.AWBCode,
		CarrierName:     h.GetCarrierName(),
		InternalOrderID: payload.OrderID,
		StatusCode:      payload.CurrentStatus,
		NDRReason:       payload.NDR.Reason,
		AttemptCount:    payload.NDR.AttemptCount,
		EventTimestamp:  timestamp,
	}, nil
}
