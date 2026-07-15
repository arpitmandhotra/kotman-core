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
	
	// --- SHADOW MODE FEATURE ---
	ShadowModeEndsAt time.Time `gorm:"index"`

	// Standard tracking timestamps
	CreatedAt time.Time
	UpdatedAt time.Time

	// --- SUBSCRIPTION MODULES ---
	HasRTOEngine             bool       `gorm:"default:false"`
	HasPaidSubscription      bool       `gorm:"default:false"` // Unified subscription: Cross-Network + CRM Upsell + Kaughtman Managed WhatsApp
	PaidSubscriptionSubID    string     `gorm:"default:''"`
	PaidSubscriptionRenewsAt *time.Time // nil until purchased

	// --- SIGNALS SUBSYSTEM ---
	Vertical string `gorm:"default:''"` // "d2c_fashion" | "d2c_electronics" | "d2c_fmcg" | "d2c_beauty" | "d2c_home" | ""

	// --- FULFILLMENT SYNC QUALITY ---
	// Computed at the end of each BackfillOrderHistory run (Shopify only).
	// Fraction of orders older than 45 days that have a non-null fulfillment_status.
	// Range: 0.0 (no sync at all) to 1.0 (perfect sync).
	// NULL means not yet computed — treated as "untrusted" by the RTO proxy gate.
	// Merchants below 0.60 have the unfulfilled-paid RTO proxy suppressed to
	// prevent mislabelling delivered orders as RTOs.
	FulfillmentSyncQuality    *float64   `gorm:"type:numeric(5,4);default:null" json:"fulfillment_sync_quality,omitempty"`
	FulfillmentSyncComputedAt *time.Time `gorm:"default:null"                   json:"fulfillment_sync_computed_at,omitempty"`
}

// CrossNetworkActive returns true if the merchant has access to cross-network intelligence.
// Free for lifetime under the new pricing architecture.
func (m *Merchant) CrossNetworkActive() bool {
	return true
}

// CRMUpsellActive returns true if the merchant has access to the CRM Upsell/Recovery module.
// Free for lifetime under the new pricing architecture.
func (m *Merchant) CRMUpsellActive() bool {
	return true
}

// WhatsAppMessagingActive returns true if the merchant is eligible to use Kaughtman-managed WhatsApp messaging.
// Gated under the Paid Tier (requires paid subscription or active 30-day trial).
func (m *Merchant) WhatsAppMessagingActive() bool {
	return m.HasPaidSubscription || time.Now().Before(m.CreatedAt.AddDate(0, 0, 30))
}

// InActiveMode returns true if this merchant should have live RTO enforcement running.
func (m *Merchant) InActiveMode() bool {
	return m.HasRTOEngine
}

type ExecutionMode string

const (
	ExecutionModeShadow ExecutionMode = "SHADOW"
	ExecutionModeActive ExecutionMode = "ACTIVE"
)

// OrderAudit stores passively ingested payload for AI training in Shadow/Active modes
type OrderAudit struct {
	ID                 string        `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID         string        `gorm:"index;not null"`
	OrderID            string        `gorm:"index;not null"`
	RawPayload         string        `gorm:"type:jsonb;not null"`
	PredictedRiskScore float64       `gorm:"not null"`
	ExecutionMode      ExecutionMode `gorm:"type:varchar(20);not null"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
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

    // --- KAUGHTMAN MANAGED WALLET ---
    WalletBalancePaise int `gorm:"default:0;column:wallet_balance_paise"`

    // Billing configuration
    CheckoutMode        string `gorm:"default:'native'"` // "native" | "third_party" — merchant declares their setup
    ThirdPartyCheckout  string `gorm:"default:''"` // "gokwik" | "shopflo" | "razorpay_magic" | ""
    BillingCycleDay     int    `gorm:"default:1"` // day of month invoices are generated (1 = first of month)
    AutoInvoiceEnabled  bool   `gorm:"default:true"`

    // --- META ADS INTEGRATION (all nullable — feature is opt-in) ---
    // Note: MetaAccessToken is a Meta System User token (not a short-lived user token).
    // It does NOT expire unless manually revoked. We store it as plaintext for now because
    // it is not an OAuth token and AES encryption would add complexity without meaningful
    // benefit, since a database breach would already expose the public MetaPixelID.
    MetaPixelID        string `gorm:"default:''"`  // e.g. "1234567890123456"
    MetaAccessToken    string `gorm:"default:''"`  // System User access token
    MetaAdAccountID   string `gorm:"default:''"`  // e.g. "act_1234567890"
    MetaTestEventCode string `gorm:"default:''"`  // only set in staging/dev
    MetaCAPIEnabled   bool   `gorm:"column:meta_capi_enabled;default:false"` // master on/off switch

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
    WebhookSecretEncrypted    string `gorm:"type:text;default:''"` // Carrier/platform webhook secret (encrypted)

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

// InsightsResponse is the full analytics payload returned by GET /v1/merchants/insights.
// Sections are gated by subscription module. Fields that require a paid module
// are populated with zero/nil values and accompanied by a paywall flag when not purchased.
type InsightsResponse struct {

    // =========================================================
    // META — always returned, all tiers
    // =========================================================
    ExecutionMode             string    `json:"execution_mode"`              // "SHADOW" | "ACTIVE"
    ShadowDaysRemaining       int       `json:"shadow_days_remaining"`        // 0 if active
    ShadowEndsAt              time.Time `json:"shadow_ends_at"`
    TotalOrdersAnalyzed       int       `json:"total_orders_analyzed"`        // all OrderAudit rows for merchant
    DataCollectionStartedAt   time.Time `json:"data_collection_started_at"`  // merchant.CreatedAt
    MinCohortMet              bool      `json:"min_cohort_met"`               // true if total_orders_analyzed >= 50

    // =========================================================
    // UPGRADE PROMPTS — shown from day 25 onward
    // =========================================================
    ShowUpgradePrompt         bool      `json:"show_upgrade_prompt"`
    UpgradeUrgencyLevel       int       `json:"upgrade_urgency_level"`   // 1=gentle, 2=moderate, 3=urgent
    ShadowDaysPastExpiry      int       `json:"shadow_days_past_expiry"` // 0 if still in shadow or active
    SimulatedRTOSavingsINR    float64   `json:"simulated_rto_savings_inr"` // projected savings if RTO engine active
    SimulatedSavingsRangeMin  float64   `json:"simulated_savings_range_min"` // conservative
    SimulatedSavingsRangeMax  float64   `json:"simulated_savings_range_max"` // optimistic

    // =========================================================
    // MODULE ENTITLEMENTS — always returned, frontend uses these to render paywalls
    // =========================================================
    HasRTOEngine         bool `json:"has_rto_engine"`
    HasPaidSubscription  bool `json:"has_paid_subscription"`
    HasCrossNetworkIntel bool `json:"has_cross_network_intel"`
    HasCRMUpsellEngine   bool `json:"has_crm_upsell_engine"`

    // =========================================================
    // SECTION A — OWN STORE ANALYTICS
    // Always visible, all tiers, no paywall. Requires MinCohortMet = true to show real data.
    // All monetary values in INR (float64). All rates as 0.0–1.0 (not percentage).
    // =========================================================
    OwnStore OwnStoreAnalytics `json:"own_store"`

    // =========================================================
    // SECTION B — CROSS-NETWORK INTELLIGENCE
    // Requires HasCrossNetworkIntel == true for full data.
    // If false: populate teaser fields only (top-level aggregates), set CrossNetworkPaywalled = true.
    // =========================================================
    CrossNetwork         CrossNetworkAnalytics `json:"cross_network"`
    CrossNetworkPaywalled bool                 `json:"cross_network_paywalled"`

    // =========================================================
    // SECTION C — RTO ENGINE LIVE STATS
    // Only meaningful when HasRTOEngine = true AND ExecutionMode = "ACTIVE".
    // In shadow mode: show simulated/projected values with is_simulated = true.
    // =========================================================
    RTOEngine RTOEngineAnalytics `json:"rto_engine"`
}

// OwnStoreAnalytics — derived entirely from this merchant's OrderAudit and TrustProfile data.
// No cross-merchant data. Safe to show in all tiers.
type OwnStoreAnalytics struct {
    // Volume
    TotalOrdersLast30Days     int     `json:"total_orders_last_30_days"`
    CODOrdersLast30Days       int     `json:"cod_orders_last_30_days"`
    CODShareRate              float64 `json:"cod_share_rate"`              // COD / total

    // Spend profile of this merchant's buyers (derived from OrderAudit.RawPayload cart values)
    AvgCartValueINR           float64 `json:"avg_cart_value_inr"`
    MedianCartValueINR        float64 `json:"median_cart_value_inr"`
    CartValueP25INR           float64 `json:"cart_value_p25_inr"`          // 25th percentile
    CartValueP75INR           float64 `json:"cart_value_p75_inr"`          // 75th percentile
    CartValueP90INR           float64 `json:"cart_value_p90_inr"`          // 90th percentile

    // RTO on this store
    ObservedRTORate           float64 `json:"observed_rto_rate"`           // RTOs / total delivered
    ObservedRTOCount          int     `json:"observed_rto_count"`
    EstimatedRTOCostINR       float64 `json:"estimated_rto_cost_inr"`      // count * 280 (avg fwd+rev shipping)

    // Buyer intent distribution (from PredictedRiskScore in OrderAudit)
    // Risk score 0-100 where 100 = safest. Bucketed into 3 tiers.
    BuyerIntentDistribution   BuyerIntentBuckets `json:"buyer_intent_distribution"`

    // Kaughtman Score average for this merchant's buyers
    // Kaughtman Score = average PredictedRiskScore across all OrderAudit rows for this merchant
    // Labelled as: 0-39 = "High Risk", 40-69 = "Moderate", 70-84 = "Trusted", 85-100 = "VIP"
    AvgKaughtmanScore            float64 `json:"avg_kaughtman_score"`
    KaughtmanScoreLabel          string  `json:"kaughtman_score_label"` // "High Risk" | "Moderate" | "Trusted" | "VIP"

    // Refund/complaint rate on own store
    // Derived from CustomerFeedback rows where merchant_id = this merchant
    OwnStoreRefundRate        float64 `json:"own_store_refund_rate"`       // complaints / total_orders
    OwnStoreRefundCount       int     `json:"own_store_refund_count"`
    TopComplaintCategory      string  `json:"top_complaint_category"`      // most frequent Category in CustomerFeedback

    // Pincode breakdown (top 5 pincodes by order volume for this merchant)
    // Each entry: { pincode, order_count, rto_rate, avg_cart_inr }
    TopPincodesByVolume       []PincodeInsight `json:"top_pincodes_by_volume"`
}

// BuyerIntentBuckets — how this merchant's buyer base splits across risk tiers
type BuyerIntentBuckets struct {
    HighRiskPercent    float64 `json:"high_risk_pct"`    // score 0–39, label "Impulsive / At-Risk"
    ModeratePercent    float64 `json:"moderate_pct"`     // score 40–69, label "Casual Buyer"
    TrustedPercent     float64 `json:"trusted_pct"`      // score 70–84, label "Trusted"
    VIPPercent         float64 `json:"vip_pct"`          // score 85–100, label "VIP"
    // Counts
    HighRiskCount      int     `json:"high_risk_count"`
    ModerateCount      int     `json:"moderate_count"`
    TrustedCount       int     `json:"trusted_count"`
    VIPCount           int     `json:"vip_count"`
}

// PincodeInsight — per-pincode breakdown
type PincodeInsight struct {
    Pincode       string  `json:"pincode"`
    OrderCount    int     `json:"order_count"`
    RTORate       float64 `json:"rto_rate"`
    AvgCartINR    float64 `json:"avg_cart_inr"`
}

// CrossNetworkAnalytics — aggregate statistics across ALL merchants in the Kaughtman network.
// CRITICAL DPDP RULE: every field here is a STATISTICAL AGGREGATE with minimum cohort of 50.
// No individual buyer is identifiable from any field in this struct.
// Individual phone hashes are never surfaced. Only distributions and percentiles.
type CrossNetworkAnalytics struct {
    // How this merchant's buyers compare to the full network
    // Spending percentile: what % of network buyers spend LESS than this merchant's avg buyer
    MerchantSpendingPercentile    float64 `json:"merchant_spending_percentile"` // 0.0–100.0
    NetworkAvgCartINR             float64 `json:"network_avg_cart_inr"`
    NetworkMedianCartINR          float64 `json:"network_median_cart_inr"`

    // Top 10% spenders across the entire network
    // "What do the top 10% of buyers across all Kaughtman merchants spend per order on average?"
    NetworkTop10PctAvgCartINR     float64 `json:"network_top10_pct_avg_cart_inr"`
    NetworkTop10PctAvgMonthlyINR  float64 `json:"network_top10_pct_avg_monthly_inr"` // estimated monthly spend (avg_cart * avg_order_frequency)

    // Buyer overlap: what % of this merchant's buyers exist elsewhere in the network
    // and what is the aggregate spend of the overlapping segment
    NetworkOverlapPct             float64 `json:"network_overlap_pct"`          // 0.0–1.0
    OverlapBuyersAvgMonthlyINR    float64 `json:"overlap_buyers_avg_monthly_inr"` // avg monthly spend of overlapping buyers across network
    OverlapBuyersAvgCartINR       float64 `json:"overlap_buyers_avg_cart_inr"`

    // Spending band distribution: of this merchant's buyers, what % fall into each network spend band
    // Bands: Low (<₹500), Mid (₹500–₹2000), High (₹2000–₹5000), Premium (>₹5000)
    SpendBandDistribution         SpendBandBreakdown `json:"spend_band_distribution"`

    // Refund rate across network (not merchant-specific — entire network aggregate)
    // "Across all Kaughtman merchants, X% of orders result in a refund/RTO"
    NetworkRTORateAggregate       float64 `json:"network_rto_rate_aggregate"`   // RTOs / total_orders across all merchants
    NetworkRefundRateAggregate    float64 `json:"network_refund_rate_aggregate"` // CustomerFeedback complaints / total_orders network-wide
    // For this merchant specifically vs network
    MerchantRTOVsNetworkDelta     float64 `json:"merchant_rto_vs_network_delta"` // positive = merchant worse than network

    // Kaughtman Score comparison
    MerchantAvgKaughtmanScore        float64 `json:"merchant_avg_kaughtman_score"`    // same as OwnStore.AvgKaughtmanScore
    NetworkAvgKaughtmanScore         float64 `json:"network_avg_kaughtman_score"`     // across all merchants

    // TEASER ONLY (available in free tier, full data requires module)
    // Teaser = aggregate figures above without SpendBandDistribution detail or overlap breakdowns
    IsTeaserOnly                  bool    `json:"is_teaser_only"` // true when CrossNetworkPaywalled

    // Cohort note: only populated when network has >= 50 merchant-buyers with overlap
    NetworkCohortSufficient       bool    `json:"network_cohort_sufficient"`
}

// SpendBandBreakdown — what % of this merchant's buyers fall into each network spend tier
type SpendBandBreakdown struct {
    LowPct     float64 `json:"low_pct"`     // <₹500 avg cart
    MidPct     float64 `json:"mid_pct"`     // ₹500–₹2,000
    HighPct    float64 `json:"high_pct"`    // ₹2,000–₹5,000
    PremiumPct float64 `json:"premium_pct"` // >₹5,000
}

// RTOEngineAnalytics — live engine stats. Populated in ACTIVE mode, simulated in SHADOW mode.
type RTOEngineAnalytics struct {
    IsSimulated              bool    `json:"is_simulated"` // true during shadow mode
    OrdersEvaluatedTotal     int     `json:"orders_evaluated_total"`
    OrdersBlockedTotal       int     `json:"orders_blocked_total"`    // COD hidden or WA intervention triggered
    BlockRate                float64 `json:"block_rate"`              // blocked / evaluated
    FalsePositiveRate        float64 `json:"false_positive_rate"`     // RequiresReview=true events / blocked (manual estimate)
    WalletBalanceINR         float64 `json:"wallet_balance_inr"`      // current wallet in INR
    EstimatedDaysWalletLeft  int     `json:"estimated_days_wallet_left"` // wallet / avg_daily_burn
    AvgDailyFeePaise         int     `json:"avg_daily_fee_paise"`
    ProjectedMonthlyFeeINR   float64 `json:"projected_monthly_fee_inr"`
    RTOsSavedTotal           int     `json:"rtos_saved_total"`        // BillableEvents where block happened AND order had no delivery
    EstimatedRevenueSavedINR float64 `json:"estimated_revenue_saved_inr"` // RTOsSaved * 280
    ThreeDayTrailingBlocksINR float64 `json:"three_day_trailing_blocks_inr"`
}