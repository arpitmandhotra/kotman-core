package courier

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type DelhiveryAdapter struct{}

func NewDelhiveryAdapter() *DelhiveryAdapter {
	return &DelhiveryAdapter{}
}

func (a *DelhiveryAdapter) Provider() CourierProvider {
	return ProviderDelhivery
}

type DelhiveryPayload struct {
	Waybill      string `json:"waybill"`
	OrderID      string `json:"order_id"`
	Status       string `json:"status"` // e.g. "NDR", "DELIVERED"
	StatusDate   string `json:"status_date"`
	Instructions string `json:"instructions"`
	Reason       string `json:"reason"`
	AttemptCount int    `json:"attempt_count"`
}

func (a *DelhiveryAdapter) VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error {
	token := headers.Get("X-Delhivery-Token")
	if token == "" {
		return errors.New("missing X-Delhivery-Token header")
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(merchantSecret)) != 1 {
		return errors.New("invalid Delhivery signature token")
	}
	return nil
}

func (a *DelhiveryAdapter) ParseEvent(rawBody []byte) (RawCourierEvent, error) {
	var payload DelhiveryPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Delhivery webhook payload: %w", err)
	}
	if payload.Waybill == "" {
		return nil, errors.New("missing waybill in Delhivery payload")
	}
	return payload, nil
}

func (a *DelhiveryAdapter) Normalize(raw RawCourierEvent) (NormalizedDeliveryEvent, error) {
	payload, ok := raw.(DelhiveryPayload)
	if !ok {
		return NormalizedDeliveryEvent{}, errors.New("invalid event type for Delhivery adapter")
	}

	eventTime := time.Now()
	if payload.StatusDate != "" {
		parsed, err := time.Parse(time.RFC3339, payload.StatusDate)
		if err == nil {
			eventTime = parsed
		} else {
			parsed, err = time.Parse("2006-01-02 15:04:05", payload.StatusDate)
			if err == nil {
				eventTime = parsed
			}
		}
	}

	// Reason mapping
	reason := ReasonUnknown
	reasonLower := strings.ToLower(fmt.Sprintf("%s %s", payload.Reason, payload.Instructions))
	switch {
	case strings.Contains(reasonLower, "refused") || strings.Contains(reasonLower, "rejected") || strings.Contains(reasonLower, "not want"):
		reason = ReasonCustomerRefused
	case strings.Contains(reasonLower, "not available") || strings.Contains(reasonLower, "uncontactable") || strings.Contains(reasonLower, "unavailable") || strings.Contains(reasonLower, "locked"):
		reason = ReasonCustomerUnavailable
	case strings.Contains(reasonLower, "incorrect address") || strings.Contains(reasonLower, "wrong address") || strings.Contains(reasonLower, "pincode"):
		reason = ReasonAddressIncorrect
	case strings.Contains(reasonLower, "cod not ready") || strings.Contains(reasonLower, "no cash") || strings.Contains(reasonLower, "money"):
		reason = ReasonCODNotReady
	case strings.Contains(reasonLower, "delayed") || strings.Contains(reasonLower, "late"):
		reason = ReasonDeliveryDelayed
	}

	// Status mapping to internal DeliveryEventType
	eventType := EventInTransit
	switch payload.Status {
	case "NDR", "undelivered":
		eventType = EventNDRAttempted
	case "NDR_RESOLVED", "resolved":
		eventType = EventNDRResolved
	case "RTO", "rto_initiated":
		eventType = EventRTOInitiated
	case "RTO_DELIVERED", "rto_returned":
		eventType = EventRTODelivered
	case "DELIVERED", "delivered":
		eventType = EventDelivered
	}

	attempt := payload.AttemptCount
	if attempt == 0 {
		attempt = 1
	}

	return NormalizedDeliveryEvent{
		ID:               uuid.New(),
		AWB:              payload.Waybill,
		CourierProvider:  ProviderDelhivery,
		EventType:        eventType,
		AttemptNumber:    attempt,
		ReasonCode:       reason,
		CourierTimestamp: eventTime,
		ReceivedAt:       time.Now(),
	}, nil
}
