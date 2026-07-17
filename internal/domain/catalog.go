package domain

import (
	"time"

	"github.com/google/uuid"
)

type PlatformType string

const (
	PlatformShopify     PlatformType = "SHOPIFY"
	PlatformWooCommerce PlatformType = "WOOCOMMERCE"
	PlatformMagento     PlatformType = "MAGENTO"
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
	GeoState               string    `gorm:"default:''" json:"geo_state"`
	GeoTier                string    `gorm:"default:''" json:"geo_tier"`
	GeoDistrict            string    `gorm:"default:''" json:"geo_district"`
	GeoLatitude            float64   `gorm:"type:decimal(10,7);default:0.0" json:"geo_latitude"`
	GeoLongitude           float64   `gorm:"type:decimal(10,7);default:0.0" json:"geo_longitude"`
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

var OrderTimeDecayWeights = []struct {
	MaxAgeMonths int
	Weight       float64
}{
	{MaxAgeMonths: 6,  Weight: 1.0},
	{MaxAgeMonths: 12, Weight: 0.8},
	{MaxAgeMonths: 24, Weight: 0.5},
	{MaxAgeMonths: 36, Weight: 0.3},
}

// OrderWeight returns the time-decay weight for a given age in months.
func OrderWeight(ageMonths int) float64 {
	for _, w := range OrderTimeDecayWeights {
		if ageMonths <= w.MaxAgeMonths {
			return w.Weight
		}
	}
	return 0.3 // cap at 36-month weight, never zero
}
