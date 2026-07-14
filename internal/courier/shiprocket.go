package courier

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/security"
	"github.com/google/uuid"
)

type ShiprocketAdapter struct{}

func NewShiprocketAdapter() *ShiprocketAdapter {
	return &ShiprocketAdapter{}
}

func (a *ShiprocketAdapter) Provider() CourierProvider {
	return ProviderShiprocket
}

type ShiprocketPayload struct {
	AWBCode       string `json:"awb_code"`
	OrderID       string `json:"order_id"`
	CurrentStatus string `json:"current_status"` // e.g. "undelivered", "delivered"
	NDR           struct {
		Reason       string `json:"reason"`
		AttemptCount int    `json:"attempt_count"`
		NDRDate      string `json:"ndr_date"` // "2026-07-12 14:15:00"
	} `json:"ndr"`
}

func (a *ShiprocketAdapter) VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error {
	signature := headers.Get("X-Shiprocket-Hmac-Sha256")
	if signature == "" {
		return errors.New("missing X-Shiprocket-Hmac-Sha256 header")
	}

	if !security.ValidateHMAC(rawBody, signature, []byte(merchantSecret)) {
		return errors.New("invalid Shiprocket signature")
	}
	return nil
}

func (a *ShiprocketAdapter) ParseEvent(rawBody []byte) (RawCourierEvent, error) {
	var payload ShiprocketPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Shiprocket webhook payload: %w", err)
	}
	if payload.AWBCode == "" {
		return nil, errors.New("missing awb_code in Shiprocket payload")
	}
	return payload, nil
}

func (a *ShiprocketAdapter) Normalize(raw RawCourierEvent) (NormalizedDeliveryEvent, error) {
	payload, ok := raw.(ShiprocketPayload)
	if !ok {
		return NormalizedDeliveryEvent{}, errors.New("invalid event type for Shiprocket adapter")
	}

	eventTime := time.Now()
	if payload.NDR.NDRDate != "" {
		parsed, err := time.Parse("2006-01-02 15:04:05", payload.NDR.NDRDate)
		if err == nil {
			eventTime = parsed
		}
	}

	// Reason mapping
	reason := ReasonUnknown
	reasonLower := strings.ToLower(payload.NDR.Reason)
	switch {
	case strings.Contains(reasonLower, "refused") || strings.Contains(reasonLower, "reject"):
		reason = ReasonCustomerRefused
	case strings.Contains(reasonLower, "not available") || strings.Contains(reasonLower, "unavailable") || strings.Contains(reasonLower, "locked"):
		reason = ReasonCustomerUnavailable
	case strings.Contains(reasonLower, "address") || strings.Contains(reasonLower, "pincode") || strings.Contains(reasonLower, "incorrect"):
		reason = ReasonAddressIncorrect
	case strings.Contains(reasonLower, "cod") || strings.Contains(reasonLower, "cash") || strings.Contains(reasonLower, "money"):
		reason = ReasonCODNotReady
	case strings.Contains(reasonLower, "delay") || strings.Contains(reasonLower, "late"):
		reason = ReasonDeliveryDelayed
	}

	// Status mapping
	eventType := EventInTransit
	statusLower := strings.ToLower(payload.CurrentStatus)
	switch statusLower {
	case "undelivered", "ndr", "out_for_delivery_failed":
		eventType = EventNDRAttempted
	case "ndr_resolved", "reattempt":
		eventType = EventNDRResolved
	case "rto", "rto_initiated", "returning":
		eventType = EventRTOInitiated
	case "rto_delivered", "returned", "rto_returned":
		eventType = EventRTODelivered
	case "delivered", "completed":
		eventType = EventDelivered
	}

	attempt := payload.NDR.AttemptCount
	if attempt == 0 {
		attempt = 1
	}

	return NormalizedDeliveryEvent{
		ID:               uuid.New(),
		AWB:              payload.AWBCode,
		CourierProvider:  ProviderShiprocket,
		EventType:        eventType,
		AttemptNumber:    attempt,
		ReasonCode:       reason,
		CourierTimestamp: eventTime,
		ReceivedAt:       time.Now(),
	}, nil
}
