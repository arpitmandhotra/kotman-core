package webhooks

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DelhiveryHandler concrete strategy implementation.
type DelhiveryHandler struct{}

func NewDelhiveryHandler() *DelhiveryHandler {
	return &DelhiveryHandler{}
}

func (h *DelhiveryHandler) GetCarrierName() string {
	return "delhivery"
}

// DelhiveryRawPayload models the Delhivery webhook schema.
type DelhiveryRawPayload struct {
	Waybill    string `json:"waybill"`
	ClientName string `json:"client_name"`
	OrderID    string `json:"order_id"`
	Status     struct {
		StatusType   string    `json:"status_type"` // e.g. "NDR"
		StatusDate   time.Time `json:"status_date"`
		Instructions string    `json:"instructions"`
		Reason       string    `json:"reason"`
	} `json:"status"`
	AttemptCount int `json:"attempt_count"`
}

func (h *DelhiveryHandler) ValidateSignature(ctx context.Context, req *http.Request, body []byte, secret []byte) error {
	// Delhivery webhook security uses a static token sent in header (e.g. X-Delhivery-Token)
	providedToken := req.Header.Get("X-Delhivery-Token")
	if providedToken == "" {
		return errors.New("missing X-Delhivery-Token header")
	}

	// Constant-time check to prevent timing analysis of the token
	if subtle.ConstantTimeCompare([]byte(providedToken), secret) != 1 {
		return errors.New("invalid delhivery authentication token")
	}
	return nil
}

func (h *DelhiveryHandler) ParsePayload(ctx context.Context, body []byte) (*NormalizedNDREvent, error) {
	var payload DelhiveryRawPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal delhivery payload: %w", err)
	}

	if payload.Waybill == "" {
		return nil, errors.New("missing waybill tracking number in delhivery payload")
	}

	timestamp := payload.Status.StatusDate
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	return &NormalizedNDREvent{
		TrackingAWB:     payload.Waybill,
		CarrierName:     h.GetCarrierName(),
		InternalOrderID: payload.OrderID,
		StatusCode:      payload.Status.StatusType,
		NDRReason:       payload.Status.Reason,
		AttemptCount:    payload.AttemptCount,
		EventTimestamp:  timestamp,
	}, nil
}
