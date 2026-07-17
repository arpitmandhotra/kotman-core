package domain

import "time"

type WaitlistEntry struct {
	ID           string    `gorm:"primaryKey;type:uuid"`
	Email        string    `gorm:"uniqueIndex;not null"`
	StoreName    string    `gorm:"not null;default:''"`
	MerchantID   string    `gorm:"index;default:''"` // set if the entry comes from a logged-in merchant
	TierInterest string    `gorm:"not null;default:'growth'"` // "growth" | "growth_ads" | "both"
	Source       string    `gorm:"not null;default:'dashboard'"` // "dashboard" | "pricing_page" | "onboarding"
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (WaitlistEntry) TableName() string {
	return "waitlist_entries"
}
