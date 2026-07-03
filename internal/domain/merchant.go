package domain

import "time"

// Merchant represents a paying Shopify store in your system
type Merchant struct {
	// Your existing, highly-secure UUID setup
	ID        string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	StoreName string `gorm:"not null"`
	APIKeyHash string `gorm:"uniqueIndex;not null"`

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

    // Billing configuration
    CheckoutMode        string `gorm:"default:'native'"` // "native" | "third_party" — merchant declares their setup
    ThirdPartyCheckout  string `gorm:"default:''"` // "gokwik" | "shopflo" | "razorpay_magic" | ""
    BillingCycleDay     int    `gorm:"default:1"` // day of month invoices are generated (1 = first of month)
    AutoInvoiceEnabled  bool   `gorm:"default:true"`

    CreatedAt time.Time
    UpdatedAt time.Time
}

type TransactionHistory struct {
	ID         string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID string    `gorm:"index;not null"`
	CartValue  float64   `gorm:"not null"`
	FeeCharged float64   `gorm:"not null"`
	CreatedAt  time.Time
}

type PlatformCredential struct {
    ID              string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    MerchantID      string `gorm:"uniqueIndex:idx_merchant_platform;not null"`
    Platform        string `gorm:"uniqueIndex:idx_merchant_platform;not null"` // "shopify" | "woocommerce" | "magento"
    ShopDomain      string `gorm:"index"` // e.g. "example.myshopify.com" or store base URL for WooCommerce/Magento

    // ENCRYPTED AT REST — use AES-256-GCM via internal/crypto, never store plaintext
    AccessTokenEncrypted  string `gorm:"type:text"`  // Shopify offline access token (encrypted)
    RefreshTokenEncrypted string `gorm:"type:text"`  // Shopify refresh token (encrypted)
    ConsumerKeyEncrypted    string `gorm:"type:text"` // WooCommerce consumer key (encrypted)
    ConsumerSecretEncrypted string `gorm:"type:text"` // WooCommerce consumer secret (encrypted)
    IntegrationTokenEncrypted string `gorm:"type:text"` // Magento integration token (encrypted)

    Scopes          string    `gorm:"type:text"` // comma-separated granted scopes
    TokenExpiresAt  *time.Time `gorm:"index"` // CRITICAL for Shopify — 60 minute expiry
    LastRefreshedAt *time.Time
    InstalledAt     time.Time
    UninstalledAt   *time.Time `gorm:"index"` // set by shop/redact webhook, never hard-delete
    IsActive        bool      `gorm:"default:true"`
}

type BackfilledOrder struct {
	ID         string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID string    `gorm:"uniqueIndex:idx_merchant_order;not null"`
	Platform   string    `gorm:"not null"`
	OrderID    string    `gorm:"uniqueIndex:idx_merchant_order;not null"`
	CreatedAt  time.Time
}