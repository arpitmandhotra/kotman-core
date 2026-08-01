package domain

import "time"

// ==========================================
// INVENTORY INTELLIGENCE MODELS
// ==========================================

// InventorySnapshot captures a point-in-time stock level per merchant per SKU.
// The daily midnight IST cron job populates a row per active SKU per merchant.
type InventorySnapshot struct {
	ID                string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID        string    `gorm:"index;not null"`
	ProductID         string    `gorm:"index;not null"` // Platform-native product ID
	SKU               string    `gorm:"index;not null"`
	StockQuantity     int       `gorm:"not null;default:0"`
	WarehouseLocation string    `gorm:"not null;default:''"`
	SnapshotDate      time.Time `gorm:"type:date;index;not null"` // Midnight IST date of the snapshot
	CreatedAt         time.Time // Actual timestamp of record insertion
}

// InventoryProduct is Kaughtman's enriched product master.
// Separate from CatalogProduct (which mirrors the platform's variant catalogue).
// This table holds supply-chain metadata: reorder points, safety stock, lead times.
type InventoryProduct struct {
	ID             string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID     string    `gorm:"uniqueIndex:idx_inv_product_merchant_sku;not null"`
	SKU            string    `gorm:"uniqueIndex:idx_inv_product_merchant_sku;not null"`
	ProductName    string    `gorm:"not null;default:''"`
	Category       string    `gorm:"index;not null;default:''"`
	Price          float64   `gorm:"type:numeric(12,4);not null;default:0"`
	SupplierID     string    `gorm:"index;default:''"` // FK to InventorySupplier.ID (soft — no FK constraint)
	ReorderPoint   int       `gorm:"not null;default:0"` // Trigger replenishment when stock falls below this
	SafetyStock    int       `gorm:"not null;default:0"` // Buffer stock to absorb demand variability
	LeadTimeDays   int       `gorm:"not null;default:0"` // Days from PO to warehouse receipt
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// InventorySupplier stores supplier relationship metadata per merchant.
type InventorySupplier struct {
	ID                  string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID          string    `gorm:"index;not null"`
	SupplierName        string    `gorm:"not null;default:''"`
	LeadTimeDays        int       `gorm:"not null;default:0"`
	MinimumOrderQty     int       `gorm:"not null;default:0"` // MOQ
	ContactInfo         string    `gorm:"type:jsonb;not null;default:'{}'"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// SalesEvent represents a planned promotional or campaign period for a merchant.
// Used for demand forecasting uplift adjustments in inventory planning.
type SalesEvent struct {
	ID                    string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	MerchantID            string    `gorm:"index;not null"`
	EventName             string    `gorm:"not null;default:''"`
	EventType             string    `gorm:"type:varchar(30);not null;default:'sale'"` // festival | campaign | sale
	StartDate             time.Time `gorm:"type:date;index;not null"`
	EndDate               time.Time `gorm:"type:date;not null"`
	DiscountPercentage    float64   `gorm:"type:numeric(5,2);not null;default:0"`
	ExpectedUpliftPercent float64   `gorm:"type:numeric(5,2);not null;default:0"` // Expected demand uplift %
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// ==========================================
// FESTIVAL CALENDAR
// ==========================================

// FestivalCalendar stores known Indian D2C commerce events with their expected dates.
// Fixed-date events (e.g. Republic Day, Independence Day) have a deterministic date.
// Variable-date events (e.g. Diwali, Holi, Dussehra) are seeded per calendar year.
type FestivalCalendar struct {
	ID                    string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
	FestivalName          string    `gorm:"uniqueIndex:idx_festival_name_year;not null"`
	Year                  int       `gorm:"uniqueIndex:idx_festival_name_year;not null"`
	EventType             string    `gorm:"type:varchar(30);not null;default:'festival'"` // festival | sale
	StartDate             time.Time `gorm:"type:date;not null"`
	EndDate               time.Time `gorm:"type:date;not null"`
	IsDateFixed           bool      `gorm:"not null;default:false"`  // true = same Gregorian date every year
	ExpectedUpliftPercent float64   `gorm:"type:numeric(5,2);not null;default:0"` // Platform-wide average uplift baseline
	Notes                 string    `gorm:"type:text;default:''"`
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// ==========================================
// WEBHOOK EVENT TYPES (reference constants)
// ==========================================

// Shopify inventory webhook topics
const (
	ShopifyTopicInventoryLevelUpdate = "inventory_levels/update"
	ShopifyTopicInventoryItemUpdate  = "inventory_items/update"
)

// WooCommerce inventory webhook action
const WooTopicProductUpdated = "woocommerce_product_object_updated"

// Magento inventory webhook event name
const MagentoEventCatalogProductSaveAfter = "catalog_product_save_after"
