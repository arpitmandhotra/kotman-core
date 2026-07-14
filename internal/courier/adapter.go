package courier

import (
	"net/http"
)

type RawCourierEvent interface{}

// CourierWebhookAdapter defines the contract every courier partner integration must implement.
type CourierWebhookAdapter interface {
	Provider() CourierProvider
	VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error
	ParseEvent(rawBody []byte) (RawCourierEvent, error)
	Normalize(event RawCourierEvent) (NormalizedDeliveryEvent, error)
}
