package courier

import (
	"errors"
	"net/http"
)

type ClickpostAdapter struct{}

func NewClickpostAdapter() *ClickpostAdapter {
	return &ClickpostAdapter{}
}

func (a *ClickpostAdapter) Provider() CourierProvider {
	return ProviderClickpost
}

func (a *ClickpostAdapter) VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error {
	// TODO: confirm Clickpost API signature scheme once commercial relationship is verified.
	return nil
}

func (a *ClickpostAdapter) ParseEvent(rawBody []byte) (RawCourierEvent, error) {
	return nil, errors.New("clickpost parser not implemented")
}

func (a *ClickpostAdapter) Normalize(event RawCourierEvent) (NormalizedDeliveryEvent, error) {
	return NormalizedDeliveryEvent{}, errors.New("clickpost normalizer not implemented")
}
