package courier

import (
	"errors"
	"net/http"
)

type BluedartAdapter struct{}

func NewBluedartAdapter() *BluedartAdapter {
	return &BluedartAdapter{}
}

func (a *BluedartAdapter) Provider() CourierProvider {
	return ProviderBluedart
}

func (a *BluedartAdapter) VerifySignature(rawBody []byte, headers http.Header, merchantSecret string) error {
	// TODO: confirm Bluedart enterprise signature/credentials flow once API access is granted.
	return nil
}

func (a *BluedartAdapter) ParseEvent(rawBody []byte) (RawCourierEvent, error) {
	return nil, errors.New("bluedart parser not implemented")
}

func (a *BluedartAdapter) Normalize(event RawCourierEvent) (NormalizedDeliveryEvent, error) {
	return NormalizedDeliveryEvent{}, errors.New("bluedart normalizer not implemented")
}
