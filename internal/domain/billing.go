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
	RequiresReview  bool       `gorm:"default:false"`
	CreatedAt       time.Time  `gorm:"index"`
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

// KotmanFee resolves the transaction fee strictly on the 'orderValuePaise' bounds.
func KotmanFee(orderValuePaise int) int {
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
