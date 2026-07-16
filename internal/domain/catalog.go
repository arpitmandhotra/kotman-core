package domain

import (
	"time"

	"github.com/google/uuid"
)

type PlatformType string

const (
	PlatformShopify     PlatformType = "SHOPIFY"
	PlatformWooCommerce PlatformType = "WOOCOMMERCE"
)

// Decimal maps float64 value directly to numeric database columns.
type Decimal float64

type CatalogProduct struct {
	ID                uuid.UUID    `gorm:"type:uuid;primaryKey"`
	MerchantID        uuid.UUID    `gorm:"type:uuid;uniqueIndex:idx_merchant_platform_variant;not null"`
	Platform          PlatformType `gorm:"uniqueIndex:idx_merchant_platform_variant;not null"`
	PlatformProductID string       `gorm:"index;not null"`
	PlatformVariantID string       `gorm:"uniqueIndex:idx_merchant_platform_variant;not null"`
	SKU               string       `gorm:"index;not null"`
	Title             string       `gorm:"not null"`
	CategoryL1        string       `gorm:"index;default:''"`
	CategoryL2        string       `gorm:"default:''"`
	Price             Decimal      `gorm:"type:numeric(12,4);not null"`
	IsArchived        bool         `gorm:"default:false"`
	LastSyncedAt      time.Time    `gorm:"index"`
}

type Order struct {
	ID                   uuid.UUID `gorm:"type:uuid;primaryKey"`
	MerchantID           uuid.UUID `gorm:"type:uuid;index;not null"`
	OrderNumber          string    `gorm:"index;not null"`
	DeliveryStatus       string    `gorm:"default:''"`
	NDRAttempts          int       `gorm:"default:0"`
	CreatedAt            time.Time `gorm:"index"`
	BuyerPhoneNormalized   string    `gorm:"index;default:''" json:"buyer_phone_normalized"`
	BuyerEmail             string    `gorm:"index;default:''" json:"buyer_email"`
	Outcome                string    `gorm:"default:''" json:"outcome"`
	FulfillmentStatus      string    `gorm:"default:''" json:"fulfillment_status"`
	PaymentMethod          string    `gorm:"default:''" json:"payment_method"`
	OrderValuePaise        int       `gorm:"default:0" json:"order_value_paise"`
	ShippingAddressPincode string    `gorm:"default:''" json:"shipping_address_pincode"`
	City                   string    `gorm:"default:''" json:"city"`
	State                  string    `gorm:"default:''" json:"state"`
}

type OrderLineItem struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrderID    uuid.UUID `gorm:"type:uuid;index;not null"`
	VariantID  string    `gorm:"index;not null"` // joins with CatalogProduct.PlatformVariantID
	SKU        string    `gorm:"not null"`
	Quantity   int       `gorm:"default:1"`
	Price      Decimal   `gorm:"type:numeric(12,4);not null"`
	CategoryL1 string    `gorm:"index;default:''"` // Immutable Category L1 snapshot at order time
	CategoryL2 string    `gorm:"default:''"`       // Immutable Category L2 snapshot at order time
}
