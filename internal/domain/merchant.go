package domain

import "time"

// Merchant represents a paying Shopify store in your system
type Merchant struct {
	ID        string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	StoreName string    `gorm:"not null"`
	APIKey    string    `gorm:"uniqueIndex;not null"`
	CreatedAt time.Time
}