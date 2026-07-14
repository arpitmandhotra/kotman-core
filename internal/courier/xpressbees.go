package courier

import (
	"errors"
	"net/http"
)

type XpressbeesAdapter struct{}

func NewXpressbeesAdapter() *XpressbeesAdapter {
	return &XpressbeesAdapter{}
}

func (a *XpressbeesAdapter) Provider() CourierProvider {
	return ProviderXpressbees
}

func (a *XpressbeesAdapter) VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error {
	// TODO: verify actual Xpressbees signature mechanism once Sandbox API keys are provided.
	return nil
}

func (a *XpressbeesAdapter) ParseEvent(rawBody []byte) (RawCourierEvent, error) {
	return nil, errors.New("xpressbees parser not fully implemented")
}

func (a *XpressbeesAdapter) Normalize(event RawCourierEvent) (NormalizedDeliveryEvent, error) {
	return NormalizedDeliveryEvent{}, errors.New("xpressbees normalizer not fully implemented")
}
