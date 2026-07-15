package domain

import (
	"time"

	"gorm.io/gorm"
)

type BillableEvent struct {
	gorm.Model
	MerchantID      string     `gorm:"uniqueIndex:idx_merchant_platform_order;not null"`
	OrderID         string     `gorm:"uniqueIndex:idx_merchant_platform_order;not null"` // platform's native order ID
	Platform        string     `gorm:"uniqueIndex:idx_merchant_platform_order;not null"` // "shopify" | "woocommerce" | "magento" | "custom"
	CheckoutMode    string     `gorm:"not null"`                                         // "native" | "third_party"
	ThirdPartyName  string     `gorm:"default:''"`                                       // "gokwik" | "shopflo" | "razorpay_magic" | "" if native
	PaymentMethod   string     `gorm:"not null"`                                         // "cod" | "prepaid"
	OrderValuePaise int        `gorm:"not null"`
	FeePaise        int        `gorm:"not null"` // 0 if not billable
	IsBillable      bool       `gorm:"not null;default:false"`
	BilledAt        *time.Time `gorm:"index"` // nil until invoiced
	InvoiceID       string     `gorm:"index;default:''"`
	RawWebhookBody  string     `gorm:"type:text"` // stores raw JSON for dispute resolution; MUST be redacted via GDPR webhooks. Only FeePaise, OrderValuePaise, PhoneHash, and CheckoutMode are permanent.
	PhoneHash       string     `gorm:"index"`     // for cross-referencing TrustProfile
	// PhoneHashMeta stores a standard SHA-256 of the raw phone number WITHOUT
	// the Kaughtman pepper, because Meta's identity graph requires plain SHA-256
	// for cross-device matching. Unlike PhoneHash (peppered HMAC, reversible
	// only with the pepper), SHA-256 without a pepper is theoretically
	// vulnerable to rainbow-table attacks given India's finite 10-digit phone
	// space. Mitigation: this hash is stored but never logged, never exposed
	// in API responses, and access-controlled at the DB level. Merchants must
	// disclose this in their privacy policy under "third-party ad enrichment."
	PhoneHashMeta   string     `gorm:"index;default:''"` // SHA-256 (no pepper) for Meta CAPI
	RequiresReview  bool       `gorm:"default:false"`
	// --- SIGNALS SUBSYSTEM (additive — do not modify existing fields above) ---
	IsRTO        bool   `gorm:"default:false"`           // true when order is confirmed as RTO/returned via ProcessOrderCreditBack
	CategoryL1   string `gorm:"index;default:''"`         // "Apparel", "Electronics", "Footwear", "Cosmetics", "Home", "FMCG"
	CategoryL2   string `gorm:"default:''"`               // "Ethnic Wear", "Smartphones", "Running Shoes", etc.
	GeoState     string `gorm:"index;default:''"`         // extracted from shipping_address.province in the webhook JSON
	GeoTier      int    `gorm:"default:0"`                // 1, 2, or 3 — derived from GeoState via geo_tier lookup
	CreatedAt    time.Time  `gorm:"index"`
}

type MerchantInvoice struct {
	gorm.Model
	MerchantID         string     `gorm:"index;not null"`
	InvoiceNumber      string     `gorm:"uniqueIndex;not null"` // KTM-{merchantID[:8]}-{YYYYMM}-{sequence}
	BillingPeriodStart time.Time  `gorm:"not null"`
	BillingPeriodEnd   time.Time  `gorm:"not null"`
	TotalEventCount    int        `gorm:"not null"`
	TotalFeePaise      int        `gorm:"not null"`
	Status             string     `gorm:"default:'pending'"` // "pending" | "sent" | "paid" | "disputed" | "waived"
	SentAt             *time.Time
	PaidAt             *time.Time
	RazorpayOrderID    string     `gorm:"default:''"`
	Notes              string     `gorm:"type:text"`
}

type MerchantBillingAccumulator struct {
	gorm.Model
	MerchantID    string `gorm:"uniqueIndex:idx_merchant_month;not null"`
	BillingMonth  string `gorm:"uniqueIndex:idx_merchant_month;not null"` // "2026-07" format
	TotalEvents   int    `gorm:"default:0"`
	TotalFeePaise int    `gorm:"default:0"`
	IsInvoiced    bool   `gorm:"default:false"`
}

// KaughtmanFee resolves the transaction fee strictly on the 'orderValuePaise' bounds.
func KaughtmanFee(orderValuePaise int) int {
	switch {
	case orderValuePaise <= 50000:
		return 500 // ≤ ₹500 → ₹5.00
	case orderValuePaise <= 100000:
		return 750 // ≤ ₹1,000 → ₹7.50
	case orderValuePaise <= 200000:
		return 1000 // ≤ ₹2,000 → ₹10.00
	case orderValuePaise <= 300000:
		return 2000 // ≤ ₹3,000 → ₹20.00
	case orderValuePaise <= 400000:
		return 3000 // ≤ ₹4,000 → ₹30.00
	case orderValuePaise <= 500000:
		return 4000 // ≤ ₹5,000 → ₹40.00
	case orderValuePaise <= 1000000:
		return 5000 // ≤ ₹10,000 → ₹50.00
	default:
		return 10000 // > ₹10,000 → ₹100.00
	}
}

// MerchantSubscription tracks flat-fee monthly module subscriptions.
// One row per merchant per module. Updated on renewal.
type MerchantSubscription struct {
	gorm.Model
	MerchantID         string     `gorm:"uniqueIndex:idx_merchant_module;not null"`
	Module             string     `gorm:"uniqueIndex:idx_merchant_module;not null"` // "unified_paid"
	Status             string     `gorm:"default:'inactive'"` // "active" | "inactive" | "cancelled"
	PriceINR           int        `gorm:"not null"`           // 4999
	RazorpaySubID      string     `gorm:"default:''"`         // Razorpay subscription ID if recurring
	RazorpayOrderID    string     `gorm:"default:''"`         // for one-time payment flow
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
	CancelledAt        *time.Time
}

// Module name constants
const (
	ModuleCrossNetwork = "cross_network" // Deprecated: standalone modules clubbed
	ModuleCRMUpsell    = "crm_upsell"    // Deprecated: standalone modules clubbed
	ModuleUnifiedPaid  = "unified_paid"  // Unified subscription module
)

type WhatsAppMessageLog struct {
	gorm.Model
	MerchantID string    `gorm:"index;not null"`
	PhoneHash  string    `gorm:"index;not null"`
	CostPaise  int       `gorm:"not null;default:200"` // default 200 paise (₹2.00) per message
	SentAt     time.Time `gorm:"index"`
}
