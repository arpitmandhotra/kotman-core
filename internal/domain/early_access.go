package domain

import "time"

// EarlyAccessFeature identifies a locked dashboard feature.
// It is stored as a plain VARCHAR with a DB-level CHECK constraint
// (not a Postgres ENUM) so new features can be added by updating
// the constraint — no column type migration required.
type EarlyAccessFeature string

const (
	// Inventory Intelligence (Phase 1)
	FeatureInventoryForecasting  EarlyAccessFeature = "inventory_forecasting"
	FeatureDemandPlanning        EarlyAccessFeature = "demand_planning"
	FeatureLogisticsOptimisation EarlyAccessFeature = "logistics_optimisation"
	FeatureInventoryHealth       EarlyAccessFeature = "inventory_health"
	FeatureSupplierManagement    EarlyAccessFeature = "supplier_management"

	// Buyer & Network Intelligence
	FeatureBuyerIntelligence EarlyAccessFeature = "buyer_intelligence"
	FeatureNetworkIntel      EarlyAccessFeature = "network_intel"

	// Platform Features
	FeatureModelPerformance EarlyAccessFeature = "model_performance"
	FeatureGrowthAds        EarlyAccessFeature = "growth_ads"
	FeatureCRMEnrichment    EarlyAccessFeature = "crm_enrichment"
)

// ValidEarlyAccessFeatures is the single source of truth for valid feature keys.
// Both the HTTP handler (input validation) and the DB CHECK constraint are
// derived from this set. To add a future feature:
//  1. Add a constant above.
//  2. Add the key here.
//  3. Update the CHECK constraint string in database/postgres.go.
//
// No column-type migration is needed.
var ValidEarlyAccessFeatures = map[EarlyAccessFeature]bool{
	FeatureInventoryForecasting:  true,
	FeatureDemandPlanning:        true,
	FeatureLogisticsOptimisation: true,
	FeatureInventoryHealth:       true,
	FeatureSupplierManagement:    true,
	FeatureBuyerIntelligence:     true,
	FeatureNetworkIntel:          true,
	FeatureModelPerformance:      true,
	FeatureGrowthAds:             true,
	FeatureCRMEnrichment:         true,
}

// EarlyAccessStatus tracks where a request sits in the approval workflow.
type EarlyAccessStatus string

const (
	EarlyAccessPending  EarlyAccessStatus = "pending"
	EarlyAccessApproved EarlyAccessStatus = "approved"
	EarlyAccessNotified EarlyAccessStatus = "notified"
)

// EarlyAccessRequest captures a single early-access sign-up from a visitor or
// logged-in merchant. The unique constraint on (email, feature_name) prevents
// duplicate entries. feature_name is a VARCHAR(100) backed by a CHECK constraint
// (not a Postgres ENUM) so new features require only a constraint update.
type EarlyAccessRequest struct {
	ID          string             `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID  string             `gorm:"index;default:''"` // nullable — empty when unauthenticated
	Email       string             `gorm:"uniqueIndex:idx_early_access_email_feature;not null"`
	FeatureName EarlyAccessFeature `gorm:"uniqueIndex:idx_early_access_email_feature;type:varchar(100);not null"`
	Status      EarlyAccessStatus  `gorm:"type:varchar(20);not null;default:'pending'"`
	RequestedAt time.Time          `gorm:"index;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
