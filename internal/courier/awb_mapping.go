package courier

import (
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// Re-export type definitions to keep package structures clean
type CourierProvider = domain.CourierProvider

const (
	ProviderDelhivery  CourierProvider = domain.ProviderDelhivery
	ProviderShiprocket CourierProvider = domain.ProviderShiprocket
	ProviderXpressbees CourierProvider = domain.ProviderXpressbees
	ProviderBluedart   CourierProvider = domain.ProviderBluedart
	ProviderClickpost  CourierProvider = domain.ProviderClickpost
)

type DeliveryEventType = domain.DeliveryEventType

const (
	EventNDRAttempted DeliveryEventType = domain.EventNDRAttempted
	EventNDRResolved  DeliveryEventType = domain.EventNDRResolved
	EventRTOInitiated DeliveryEventType = domain.EventRTOInitiated
	EventRTODelivered DeliveryEventType = domain.EventRTODelivered
	EventDelivered    DeliveryEventType = domain.EventDelivered
	EventInTransit    DeliveryEventType = domain.EventInTransit
)

type InternalReasonCode = domain.InternalReasonCode

const (
	ReasonCustomerRefused     InternalReasonCode = domain.ReasonCustomerRefused
	ReasonCustomerUnavailable InternalReasonCode = domain.ReasonCustomerUnavailable
	ReasonAddressIncorrect    InternalReasonCode = domain.ReasonAddressIncorrect
	ReasonCODNotReady         InternalReasonCode = domain.ReasonCODNotReady
	ReasonDeliveryDelayed     InternalReasonCode = domain.ReasonDeliveryDelayed
	ReasonUnknown             InternalReasonCode = domain.ReasonUnknown
)

type NormalizedDeliveryEvent = domain.NormalizedDeliveryEvent
type AWBMapping = domain.AWBMapping
type ProcessedWebhookEvent = domain.ProcessedWebhookEvent
