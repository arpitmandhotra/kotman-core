package inventory

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SnapshotService handles inventory data collection and persistence.
type SnapshotService struct {
	pg *gorm.DB
}

// NewSnapshotService creates a new SnapshotService.
func NewSnapshotService(pg *gorm.DB) *SnapshotService {
	return &SnapshotService{pg: pg}
}

// UpsertSnapshotFromShopify processes an inventory_levels/update or inventory_items/update
// Shopify webhook payload and upserts an InventorySnapshot record.
//
// Shopify inventory_levels/update payload shape:
//
//	{
//	  "inventory_item_id": 271878346596884015,
//	  "location_id":       905684977,
//	  "available":         42,
//	  "updated_at":        "2025-01-01T12:00:00Z"
//	}
//
// Shopify inventory_items/update payload shape:
//
//	{
//	  "id":  271878346596884015,
//	  "sku": "XYZ-001",
//	  ...
//	}
func (s *SnapshotService) UpsertSnapshotFromShopify(ctx context.Context, merchantID, topic string, raw []byte) {
	switch topic {
	case domain.ShopifyTopicInventoryLevelUpdate:
		s.processShopifyLevelUpdate(ctx, merchantID, raw)
	case domain.ShopifyTopicInventoryItemUpdate:
		s.processShopifyItemUpdate(ctx, merchantID, raw)
	default:
		slog.Debug("inventory: unhandled shopify topic", "topic", topic, "merchant_id", merchantID)
	}
}

func (s *SnapshotService) processShopifyLevelUpdate(ctx context.Context, merchantID string, raw []byte) {
	var payload struct {
		InventoryItemID json.Number `json:"inventory_item_id"`
		LocationID      json.Number `json:"location_id"`
		Available       int         `json:"available"`
		UpdatedAt       string      `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Error("inventory: failed to parse shopify inventory_level/update", "error", err, "merchant_id", merchantID)
		return
	}

	productID := payload.InventoryItemID.String()
	warehouseLoc := payload.LocationID.String()

	snapshot := domain.InventorySnapshot{
		MerchantID:        merchantID,
		ProductID:         productID,
		SKU:               "", // SKU not present in level/update; resolved later via inventory_items/update
		StockQuantity:     payload.Available,
		WarehouseLocation: warehouseLoc,
		SnapshotDate:      truncateToIST(time.Now()),
	}

	if err := s.upsertSnapshot(ctx, snapshot); err != nil {
		slog.Error("inventory: upsert failed for shopify level update",
			"error", err, "merchant_id", merchantID, "product_id", productID)
		return
	}

	slog.Info("inventory: snapshot updated from shopify level webhook",
		"merchant_id", merchantID, "product_id", productID, "quantity", payload.Available)
}

func (s *SnapshotService) processShopifyItemUpdate(ctx context.Context, merchantID string, raw []byte) {
	var payload struct {
		ID  json.Number `json:"id"`
		SKU string      `json:"sku"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Error("inventory: failed to parse shopify inventory_item/update", "error", err, "merchant_id", merchantID)
		return
	}

	productID := payload.ID.String()
	if payload.SKU == "" {
		slog.Debug("inventory: shopify item update has no SKU, skipping snapshot", "product_id", productID)
		return
	}

	// Back-fill SKU on any snapshot recorded today for this item that was missing it
	today := truncateToIST(time.Now())
	if err := s.pg.WithContext(ctx).
		Model(&domain.InventorySnapshot{}).
		Where("merchant_id = ? AND product_id = ? AND snapshot_date = ? AND sku = ''",
			merchantID, productID, today).
		Update("sku", payload.SKU).Error; err != nil {
		slog.Error("inventory: failed to back-fill SKU on snapshot",
			"error", err, "merchant_id", merchantID, "product_id", productID)
	}
}

// UpsertSnapshotFromWooCommerce processes a woocommerce_product_object_updated event.
//
// Payload shape (WooCommerce product object):
//
//	{
//	  "id":         1234,
//	  "sku":        "WC-001",
//	  "stock_quantity": 20,
//	  ...
//	}
func (s *SnapshotService) UpsertSnapshotFromWooCommerce(ctx context.Context, merchantID string, raw []byte) {
	var payload struct {
		ID            json.Number `json:"id"`
		SKU           string      `json:"sku"`
		StockQuantity *int        `json:"stock_quantity"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Error("inventory: failed to parse woocommerce product update", "error", err, "merchant_id", merchantID)
		return
	}

	if payload.StockQuantity == nil {
		slog.Debug("inventory: woocommerce product has no stock_quantity, skipping", "merchant_id", merchantID)
		return
	}

	snapshot := domain.InventorySnapshot{
		MerchantID:        merchantID,
		ProductID:         payload.ID.String(),
		SKU:               payload.SKU,
		StockQuantity:     *payload.StockQuantity,
		WarehouseLocation: "default",
		SnapshotDate:      truncateToIST(time.Now()),
	}

	if err := s.upsertSnapshot(ctx, snapshot); err != nil {
		slog.Error("inventory: upsert failed for woocommerce product update",
			"error", err, "merchant_id", merchantID, "sku", payload.SKU)
		return
	}

	slog.Info("inventory: snapshot updated from woocommerce product webhook",
		"merchant_id", merchantID, "sku", payload.SKU, "quantity", *payload.StockQuantity)
}

// UpsertSnapshotFromMagento processes a catalog_product_save_after event.
//
// Magento Observer payload shape:
//
//	{
//	  "sku":      "MG-SKU-001",
//	  "entity_id": 12,
//	  "qty":       50.0,
//	  ...
//	}
func (s *SnapshotService) UpsertSnapshotFromMagento(ctx context.Context, merchantID string, raw []byte) {
	var payload struct {
		EntityID json.Number `json:"entity_id"`
		SKU      string      `json:"sku"`
		Qty      *float64    `json:"qty"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Error("inventory: failed to parse magento catalog_product_save_after", "error", err, "merchant_id", merchantID)
		return
	}

	if payload.Qty == nil {
		slog.Debug("inventory: magento product has no qty, skipping", "merchant_id", merchantID)
		return
	}

	snapshot := domain.InventorySnapshot{
		MerchantID:        merchantID,
		ProductID:         payload.EntityID.String(),
		SKU:               payload.SKU,
		StockQuantity:     int(*payload.Qty),
		WarehouseLocation: "default",
		SnapshotDate:      truncateToIST(time.Now()),
	}

	if err := s.upsertSnapshot(ctx, snapshot); err != nil {
		slog.Error("inventory: upsert failed for magento product save",
			"error", err, "merchant_id", merchantID, "sku", payload.SKU)
		return
	}

	slog.Info("inventory: snapshot updated from magento product save webhook",
		"merchant_id", merchantID, "sku", payload.SKU, "quantity", int(*payload.Qty))
}

// upsertSnapshot inserts or updates an InventorySnapshot for (merchant_id, product_id, snapshot_date).
// On conflict it updates the quantity, SKU (if not blank), and warehouse_location.
func (s *SnapshotService) upsertSnapshot(ctx context.Context, snap domain.InventorySnapshot) error {
	// Build the update assignments (never blank-out SKU or location once set)
	updates := map[string]interface{}{
		"stock_quantity":     snap.StockQuantity,
		"warehouse_location": snap.WarehouseLocation,
	}
	if snap.SKU != "" {
		updates["sku"] = snap.SKU
	}

	return s.pg.WithContext(ctx).
		Model(&domain.InventorySnapshot{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "merchant_id"}, {Name: "product_id"}, {Name: "snapshot_date"}},
			DoUpdates: clause.Assignments(updates),
		}).
		Create(&snap).Error
}

// ==========================================
// DAILY MIDNIGHT IST SNAPSHOT CRON JOB
// ==========================================

// RunDailySnapshot takes a full inventory snapshot for every merchant that has at
// least one InventorySnapshot from the last 24 hours (i.e. is actively sending data).
// It is designed to be invoked at midnight IST (18:30 UTC).
//
// Strategy: for each distinct (merchant_id, product_id, sku, warehouse_location)
// combination that has a *live* stock level (sourced from the most-recent intraday
// snapshot), insert a new snapshot row dated today (IST) unless one already exists.
//
// This is idempotent — running it twice on the same calendar day is safe.
func (s *SnapshotService) RunDailySnapshot(ctx context.Context) {
	today := truncateToIST(time.Now())
	yesterday := today.AddDate(0, 0, -1)

	slog.Info("inventory cron: starting daily midnight IST snapshot", "date", today.Format("2006-01-02"))

	// Find the latest intraday state for each (merchant_id, product_id) combo
	// where we have seen data in the last 24 hours.
	type LatestLevel struct {
		MerchantID        string
		ProductID         string
		SKU               string
		StockQuantity     int
		WarehouseLocation string
	}

	var levels []LatestLevel

	// Subquery: rank rows within each (merchant_id, product_id) group by created_at DESC
	// then pick only the most-recent row (rank = 1).
	err := s.pg.WithContext(ctx).Raw(`
		SELECT DISTINCT ON (merchant_id, product_id)
			merchant_id,
			product_id,
			sku,
			stock_quantity,
			warehouse_location
		FROM inventory_snapshots
		WHERE snapshot_date >= ?
		ORDER BY merchant_id, product_id, created_at DESC
	`, yesterday).Scan(&levels).Error

	if err != nil {
		slog.Error("inventory cron: failed to query latest inventory levels", "error", err)
		return
	}

	slog.Info("inventory cron: found active SKUs to snapshot", "count", len(levels))

	inserted := 0
	for _, lvl := range levels {
		snap := domain.InventorySnapshot{
			MerchantID:        lvl.MerchantID,
			ProductID:         lvl.ProductID,
			SKU:               lvl.SKU,
			StockQuantity:     lvl.StockQuantity,
			WarehouseLocation: lvl.WarehouseLocation,
			SnapshotDate:      today,
		}

		// Use upsert so re-running the cron is always safe
		if err := s.upsertSnapshot(ctx, snap); err != nil {
			slog.Error("inventory cron: failed to insert daily snapshot",
				"merchant_id", lvl.MerchantID, "product_id", lvl.ProductID, "error", err)
			continue
		}
		inserted++
	}

	slog.Info("inventory cron: daily snapshot complete",
		"date", today.Format("2006-01-02"), "snapshots_written", inserted)
}

// StartDailyCron blocks until ctx is cancelled, firing RunDailySnapshot at midnight IST
// (18:30 UTC) each day. Call this in a dedicated goroutine from main.go.
func StartDailyCron(ctx context.Context, pg *gorm.DB) {
	svc := NewSnapshotService(pg)
	slog.Info("inventory cron: scheduler started — will fire at midnight IST daily")

	for {
		nextMidnightIST := nextMidnightIST()
		slog.Info("inventory cron: sleeping until next midnight IST",
			"next_fire", nextMidnightIST.Format(time.RFC3339))

		select {
		case <-ctx.Done():
			slog.Info("inventory cron: context cancelled — cron stopped")
			return
		case <-time.After(time.Until(nextMidnightIST)):
			svc.RunDailySnapshot(ctx)
		}
	}
}

// ==========================================
// TIME HELPERS
// ==========================================

// ist is the Indian Standard Time location (+05:30).
var ist = mustLoadIST()

func mustLoadIST() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Fallback to fixed offset if the tzdata package is not embedded
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	return loc
}

// truncateToIST returns midnight IST for the given UTC timestamp.
func truncateToIST(t time.Time) time.Time {
	istNow := t.In(ist)
	return time.Date(istNow.Year(), istNow.Month(), istNow.Day(), 0, 0, 0, 0, ist)
}

// nextMidnightIST returns the next midnight in IST from now.
func nextMidnightIST() time.Time {
	nowIST := time.Now().In(ist)
	// Advance to the next calendar day
	nextDay := nowIST.AddDate(0, 0, 1)
	return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), 0, 0, 0, 0, ist)
}
