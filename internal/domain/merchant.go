package domain

import "time"

// Merchant represents a paying Shopify store in your system
type Merchant struct {
	// Your existing, highly-secure UUID setup
	ID        string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	StoreName string `gorm:"not null"`
	APIKey    string `gorm:"uniqueIndex;not null"`

	Platform  string `gorm:"not null;default:'shopify'"`
	// --- V2 ONBOARDING UPGRADES ---
	IsActive  bool      `gorm:"default:true"` // Allows us to disable bad merchants
	
	// Standard tracking timestamps
	CreatedAt time.Time
	UpdatedAt time.Time
}
type MerchantSettings struct {
    ID         string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    MerchantID string `gorm:"uniqueIndex;not null"` // FK to Merchant.ID

    // --- CRM ROUTING ---
    // Exactly one of these should be set. Priority: CRM > OwnKey > Wallet.
    CRMProvider    string `gorm:"default:''"` // "klaviyo" | "hubspot" | "moengage" | "webengage" | ""
    CRMAPIKey      string `gorm:"default:''"` // provider API key
    CRMAccountID   string `gorm:"default:''"` // needed by MoEngage + WebEngage

    // --- BRING YOUR OWN COMMUNICATIONS KEY ---
    HasOwnCommunicationsKey bool   `gorm:"default:false"`
    ProviderAPIKey          string `gorm:"default:''"` // Twilio/Interakt key
    ProviderName            string `gorm:"default:''"` // "twilio" | "interakt"

    // --- KOTMAN MANAGED WALLET ---
    WalletBalance float64 `gorm:"default:0"`

    CreatedAt time.Time
    UpdatedAt time.Time
}