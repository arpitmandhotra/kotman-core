package domain

import "time"

// Merchant represents a paying Shopify store in your system
type Merchant struct {
	// Your existing, highly-secure UUID setup
	ID        string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	StoreName string `gorm:"not null"`
	APIKey    string `gorm:"uniqueIndex;not null"`

	// --- V2 ONBOARDING UPGRADES ---
	IsActive  bool      `gorm:"default:true"` // Allows us to disable bad merchants
	
	// Standard tracking timestamps
	CreatedAt time.Time
	UpdatedAt time.Time
}