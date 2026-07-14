package domain

import (
	"time"

	"gorm.io/gorm"
)

type ProductCatalog struct {
	gorm.Model
	MerchantID      string    `gorm:"index;not null"`
	ProductID       string    `gorm:"index;not null"`       // platform product ID
	VariantID       string    `gorm:"uniqueIndex;not null"` // platform variant ID
	Title           string    `gorm:"not null"`
	SKU             string    `gorm:"index;not null"`
	Category        string    `gorm:"index;default:''"`
	Tags            string    `gorm:"default:''"`
	PricePaise      int       `gorm:"not null"`
	CompareAtPaise  int       `gorm:"default:0"`
	LastSyncedAt    time.Time `gorm:"index"`
}

type OrderLineItem struct {
	gorm.Model
	OrderID    string `gorm:"index;not null"`
	VariantID  string `gorm:"index;not null"`
	SKU        string `gorm:"not null"`
	Quantity   int    `gorm:"default:1"`
	PricePaise int    `gorm:"not null"`
	Category   string `gorm:"default:''"`
}
