package domain

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type CourierProvider string

const (
	ProviderDelhivery  CourierProvider = "DELHIVERY"
	ProviderShiprocket CourierProvider = "SHIPROCKET"
	ProviderXpressbees CourierProvider = "XPRESSBEES"
	ProviderBluedart   CourierProvider = "BLUEDART"
	ProviderClickpost  CourierProvider = "CLICKPOST"
)

type DeliveryEventType string

const (
	EventNDRAttempted DeliveryEventType = "NDR_ATTEMPTED"
	EventNDRResolved  DeliveryEventType = "NDR_RESOLVED"
	EventRTOInitiated DeliveryEventType = "RTO_INITIATED"
	EventRTODelivered DeliveryEventType = "RTO_DELIVERED"
	EventDelivered    DeliveryEventType = "DELIVERED"
	EventInTransit    DeliveryEventType = "IN_TRANSIT"
)

type InternalReasonCode string

const (
	ReasonCustomerRefused     InternalReasonCode = "CUSTOMER_REFUSED"
	ReasonCustomerUnavailable InternalReasonCode = "CUSTOMER_UNAVAILABLE"
	ReasonAddressIncorrect    InternalReasonCode = "ADDRESS_INCORRECT"
	ReasonCODNotReady         InternalReasonCode = "COD_NOT_READY"
	ReasonDeliveryDelayed     InternalReasonCode = "DELIVERY_DELAYED"
	ReasonUnknown             InternalReasonCode = "UNKNOWN"
)

type NormalizedDeliveryEvent struct {
	ID                  uuid.UUID          `gorm:"type:uuid;primaryKey" json:"id"`
	MerchantID          uuid.UUID          `gorm:"type:uuid;index;not null" json:"merchant_id"`
	OrderID             uuid.UUID          `gorm:"type:uuid;index;not null" json:"order_id"`
	AWB                 string             `gorm:"index;not null" json:"awb"`
	CourierProvider     CourierProvider    `gorm:"not null" json:"courier_provider"`
	EventType           DeliveryEventType  `gorm:"not null" json:"event_type"`
	AttemptNumber       int                `gorm:"default:1" json:"attempt_number"`
	ReasonCode          InternalReasonCode `gorm:"default:'UNKNOWN'" json:"reason_code"`
	CourierTimestamp    time.Time          `gorm:"index;not null" json:"courier_timestamp"`
	ReceivedAt          time.Time          `gorm:"not null" json:"received_at"`
	RawPayloadEncrypted []byte             `gorm:"type:bytea" json:"raw_payload_encrypted"`
}

type AWBMapping struct {
	AWB        string          `gorm:"primaryKey" json:"awb"`
	MerchantID uuid.UUID       `gorm:"type:uuid;index;not null" json:"merchant_id"`
	OrderID    uuid.UUID       `gorm:"type:uuid;index;not null" json:"order_id"`
	Provider   CourierProvider `gorm:"not null" json:"provider"`
	CreatedAt  time.Time       `gorm:"index" json:"created_at"`
}

type ProcessedWebhookEvent struct {
	ID        uint           `gorm:"primaryKey"`
	EventHash string         `gorm:"uniqueIndex;not null"`
	CreatedAt time.Time      `gorm:"index"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}
