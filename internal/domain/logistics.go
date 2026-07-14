package domain

import (
	"time"

	"gorm.io/gorm"
)

type Order struct {
	gorm.Model
	MerchantID       string    `gorm:"index;not null"`
	PlatformOrderID  string    `gorm:"index;not null"` // e.g. Shopify order ID
	OrderNumber      string    `gorm:"index;not null"` // human readable e.g. #1001
	TrackingAWB      string    `gorm:"index;default:''"`
	CarrierName      string    `gorm:"default:''"`
	DeliveryStatus   string    `gorm:"default:''"` // e.g. "SHIPPED", "NDR_undelivered", "DELIVERED"
	NDRAttempts      int       `gorm:"default:0"`
	TotalAmountPaise int       `gorm:"not null"`
	CreatedAt        time.Time `gorm:"index"`
}

type NDRFulfillmentLog struct {
	gorm.Model
	TrackingAWB     string    `gorm:"index;not null"`
	CarrierName     string    `gorm:"not null"`
	InternalOrderID string    `gorm:"index;not null"`
	StatusCode      string    `gorm:"not null"`
	NDRReason       string    `gorm:"type:text"`
	AttemptCount    int       `gorm:"default:0"`
	EventTimestamp  time.Time `gorm:"not null"`
}
