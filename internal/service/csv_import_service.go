package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/csvimport"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type RedisClient interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

type CSVImportService struct {
	pg  *gorm.DB
	rdb RedisClient
}

func NewCSVImportService(pg *gorm.DB, rdb RedisClient) *CSVImportService {
	return &CSVImportService{pg: pg, rdb: rdb}
}

type StagedOrderRow struct {
	PhoneHash      string    `json:"phone_hash"`
	OrderValue     int       `json:"order_value"` // paise
	OrderDate      time.Time `json:"order_date"`
	OrderStatus    string    `json:"order_status"` // bucket
	OriginalStatus string    `json:"original_status"`
	OrderID        string    `json:"order_id"`
}

type ValidationReport struct {
	TotalRows                 int               `json:"total_rows"`
	AcceptedRows              int               `json:"accepted_rows"`
	RejectedRows              int               `json:"rejected_rows"`
	RejectionReasons          RejectionReasons  `json:"rejection_reasons"`
	UniqueCustomersDetected   int               `json:"unique_customers_detected"`
	HighRiskCustomersDetected int               `json:"high_risk_customers_detected"`
	PreviewToken              string            `json:"preview_token"`
}

type RejectionReasons struct {
	InvalidPhone       int `json:"invalid_phone"`
	UnparseableAmount  int `json:"unparseable_amount"`
	UnparseableDate    int `json:"unparseable_date"`
	UnrecognizedStatus int `json:"unrecognized_status"`
	DuplicateOrderID   int `json:"duplicate_order_id"`
}

// Status lifecycle values to determine which row to keep: rto > fulfilled > order_created
func statusLifecycleValue(status string) int {
	switch status {
	case "rto":
		return 3
	case "fulfilled":
		return 2
	case "order_created":
		return 1
	default:
		return 0
	}
}

// ValidateAndStage parses, normalizes, deduplicates, and stages the CSV in Redis
func (s *CSVImportService) ValidateAndStage(ctx context.Context, r io.Reader, platform string) (*ValidationReport, error) {
	reader := csv.NewReader(r)

	// Read header row
	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read CSV headers: %w", err)
	}

	// Resolve column mappings
	colIndices, err := s.mapHeaders(headers, platform)
	if err != nil {
		return nil, err
	}

	var acceptedRows []StagedOrderRow
	seenOrders := make(map[string]StagedOrderRow) // order_id -> staged row
	var rejections RejectionReasons
	totalRows := 0

	// Process CSV rows
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("skipping malformed CSV row", "error", err)
			continue
		}
		totalRows++

		// 1. Parse Phone
		phoneColVal := row[colIndices["phone"]]
		phoneHash, err := csvimport.ParsePhone(phoneColVal)
		if err != nil {
			rejections.InvalidPhone++
			continue
		}

		// 2. Parse Order Value
		valColVal := row[colIndices["order_value"]]
		valPaise, err := csvimport.ParseAmount(valColVal)
		if err != nil {
			rejections.UnparseableAmount++
			continue
		}

		// 3. Parse Order Date
		dateColVal := row[colIndices["order_date"]]
		orderDate, err := csvimport.ParseDate(dateColVal)
		if err != nil {
			rejections.UnparseableDate++
			continue
		}

		// 4. Map Order Status
		statusColVal := row[colIndices["order_status"]]
		originalStatus := statusColVal
		if platform == "shopify" {
			// Combine fulfillment and financial status for Shopify
			finStatusColVal := row[colIndices["financial_status"]]
			originalStatus = statusColVal + ":" + finStatusColVal
		}
		statusBucket, err := csvimport.MapOrderStatus(platform, originalStatus)
		if err != nil || statusBucket == "unrecognized" {
			rejections.UnrecognizedStatus++
			continue
		}

		// 5. Deduplication (order_id tracking)
		orderID := row[colIndices["order_id"]]
		newRow := StagedOrderRow{
			PhoneHash:      phoneHash,
			OrderValue:     valPaise,
			OrderDate:      orderDate,
			OrderStatus:    statusBucket,
			OriginalStatus: originalStatus,
			OrderID:        orderID,
		}

		if existingRow, ok := seenOrders[orderID]; ok {
			rejections.DuplicateOrderID++
			newLifecycle := statusLifecycleValue(newRow.OrderStatus)
			existingLifecycle := statusLifecycleValue(existingRow.OrderStatus)
			if newLifecycle > existingLifecycle {
				// Keep new row, discard existing
				seenOrders[orderID] = newRow
			}
			// If new is <= existing, we keep existing, discard new (already incremented duplicate count)
		} else {
			seenOrders[orderID] = newRow
		}
	}

	// Flatten seenOrders map into acceptedRows list
	uniqueCustomers := make(map[string]int) // phone_hash -> CSV RTO count
	for _, staged := range seenOrders {
		acceptedRows = append(acceptedRows, staged)
		if staged.OrderStatus == "rto" {
			uniqueCustomers[staged.PhoneHash]++
		} else {
			// Ensure customer is registered in unique map even if they have 0 RTOs in this CSV
			if _, exists := uniqueCustomers[staged.PhoneHash]; !exists {
				uniqueCustomers[staged.PhoneHash] = 0
			}
		}
	}

	// Identify high risk customers: existing DB RTOs + CSV RTOs > 2
	highRiskCount := 0
	for phoneHash, csvRTOs := range uniqueCustomers {
		var existingRTOs int
		var profile domain.TrustProfile
		// Read pre-existing DB data
		if err := s.pg.WithContext(ctx).Where("phone_hash = ?", phoneHash).First(&profile).Error; err == nil {
			existingRTOs = profile.TotalRTOs
		}
		if existingRTOs+csvRTOs > 2 {
			highRiskCount++
		}
	}

	// Generate preview token and stage in Redis with 30 minute TTL
	previewToken := uuid.New().String()
	redisKey := "csvimport:preview:" + previewToken
	stagedBytes, err := json.Marshal(acceptedRows)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize validation preview data: %w", err)
	}

	err = s.rdb.Set(ctx, redisKey, stagedBytes, 30*time.Minute).Err()
	if err != nil {
		return nil, fmt.Errorf("failed to stage validation preview data in Redis: %w", err)
	}

	rejectedTotal := rejections.InvalidPhone + rejections.UnparseableAmount + rejections.UnparseableDate + rejections.UnrecognizedStatus + rejections.DuplicateOrderID

	return &ValidationReport{
		TotalRows:    totalRows,
		AcceptedRows: len(acceptedRows),
		RejectedRows: rejectedTotal,
		RejectionReasons: RejectionReasons{
			InvalidPhone:       rejections.InvalidPhone,
			UnparseableAmount:  rejections.UnparseableAmount,
			UnparseableDate:    rejections.UnparseableDate,
			UnrecognizedStatus: rejections.UnrecognizedStatus,
			DuplicateOrderID:   rejections.DuplicateOrderID,
		},
		UniqueCustomersDetected:   len(uniqueCustomers),
		HighRiskCustomersDetected: highRiskCount,
		PreviewToken:              previewToken,
	}, nil
}

type CommitResult struct {
	CreatedProfiles int `json:"created_profiles"`
	UpdatedProfiles int `json:"updated_profiles"`
}

// Commit retrieves staged rows, aggregates per customer, and performs transactional upserts in GORM
func (s *CSVImportService) Commit(ctx context.Context, previewToken string, merchantID string, platform string) (*CommitResult, error) {
	redisKey := "csvimport:preview:" + previewToken

	// Fetch staged data
	stagedBytes, err := s.rdb.Get(ctx, redisKey).Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve staged validation data (token may be expired or invalid): %w", err)
	}

	var rows []StagedOrderRow
	if err := json.Unmarshal(stagedBytes, &rows); err != nil {
		return nil, fmt.Errorf("failed to deserialize staged validation data: %w", err)
	}

	if len(rows) == 0 {
		return &CommitResult{CreatedProfiles: 0, UpdatedProfiles: 0}, nil
	}

	// Aggregate metrics per customer (phone_hash)
	type AggregatedCustomer struct {
		TotalOrders          int
		SuccessfulDeliveries int
		TotalRTOs            int
		TotalCancellations   int
		TotalRevenuePaise    int
		LastActivityDate     time.Time
	}

	aggregates := make(map[string]*AggregatedCustomer)

	for _, r := range rows {
		agg, exists := aggregates[r.PhoneHash]
		if !exists {
			agg = &AggregatedCustomer{
				LastActivityDate: r.OrderDate,
			}
			aggregates[r.PhoneHash] = agg
		}

		agg.TotalOrders++

		switch r.OrderStatus {
		case "fulfilled":
			agg.SuccessfulDeliveries++
			agg.TotalRevenuePaise += r.OrderValue
		case "rto":
			// Distinguish cancellations vs RTOs
			if csvimport.IsCancellationStatus(platform, r.OriginalStatus) {
				agg.TotalCancellations++
			} else {
				agg.TotalRTOs++
			}
		}

		if r.OrderDate.After(agg.LastActivityDate) {
			agg.LastActivityDate = r.OrderDate
		}
	}

	createdCount := 0
	updatedCount := 0

	// Run GORM transaction to perform upserts
	// Explicit code comment detailing row-level deduplication vs prior backfill orders table:
	// "NOTE: Upload-local deduplication is used here. It does NOT check against the backfilled_orders table
	// from prior Shopify Admin API backfills. This is based on the architectural assumption that CSV import
	// and live API backfills are alternative paths selected by the merchant, and are not expected to run against
	// overlapping data for the same merchant."
	err = s.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for phoneHash, agg := range aggregates {
			var profile domain.TrustProfile
			err := tx.Where("phone_hash = ?", phoneHash).First(&profile).Error
			if err == nil {
				// Profile exists. Atomically add computed values.
				var newLastActivityDate *time.Time
				if profile.LastActivityDate == nil || agg.LastActivityDate.After(*profile.LastActivityDate) {
					newLastActivityDate = &agg.LastActivityDate
				} else {
					newLastActivityDate = profile.LastActivityDate
				}

				dbUpdates := map[string]interface{}{
					"total_orders":          gorm.Expr("total_orders + ?", agg.TotalOrders),
					"successful_deliveries": gorm.Expr("successful_deliveries + ?", agg.SuccessfulDeliveries),
					"total_rtos":            gorm.Expr("total_rtos + ?", agg.TotalRTOs),
					"total_cancellations":   gorm.Expr("total_cancellations + ?", agg.TotalCancellations),
					"total_revenue_spent":   gorm.Expr("total_revenue_spent + ?", agg.TotalRevenuePaise),
					"last_activity_date":    newLastActivityDate,
					"updated_at":            time.Now(),
				}

				if err := tx.Model(&profile).Updates(dbUpdates).Error; err != nil {
					return fmt.Errorf("failed to update TrustProfile for %s: %w", phoneHash, err)
				}
				updatedCount++
			} else if gorm.ErrRecordNotFound == err {
				// Profile does not exist. Insert new record.
				newProfile := domain.TrustProfile{
					PhoneHash:           phoneHash,
					FirstSeenMerchantID: merchantID,
					TotalOrders:         agg.TotalOrders,
					SuccessfulDeliveries: agg.SuccessfulDeliveries,
					TotalRTOs:           agg.TotalRTOs,
					TotalCancellations:  agg.TotalCancellations,
					TotalRevenueSpent:   agg.TotalRevenuePaise,
					LastActivityDate:    &agg.LastActivityDate,
				}
				if err := tx.Create(&newProfile).Error; err != nil {
					return fmt.Errorf("failed to create TrustProfile for %s: %w", phoneHash, err)
				}
				createdCount++
			} else {
				return fmt.Errorf("failed to query existing TrustProfile for %s: %w", phoneHash, err)
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Delete staged data from Redis upon successful commit
	s.rdb.Del(ctx, redisKey)

	return &CommitResult{
		CreatedProfiles: createdCount,
		UpdatedProfiles: updatedCount,
	}, nil
}

// mapHeaders determines the column index of required headers per platform template
func (s *CSVImportService) mapHeaders(headers []string, platform string) (map[string]int, error) {
	indices := make(map[string]int)
	processedHeaders := make([]string, len(headers))
	for i, h := range headers {
		processedHeaders[i] = strings.ToLower(strings.TrimSpace(h))
	}

	platform = strings.ToLower(platform)

	findMatch := func(aliases []string) int {
		for _, alias := range aliases {
			for idx, h := range processedHeaders {
				if h == strings.ToLower(alias) {
					return idx
				}
			}
		}
		return -1
	}

	// 1. Phone
	var phoneAliases []string
	switch platform {
	case "shopify":
		phoneAliases = []string{"phone", "billing phone", "shipping phone"}
	case "woocommerce":
		phoneAliases = []string{"billing phone", "phone", "billing_phone"}
	case "magento":
		phoneAliases = []string{"billing telephone", "telephone", "billing_telephone", "phone"}
	default:
		phoneAliases = []string{"phone", "phone number", "phone_number"}
	}
	phoneIdx := findMatch(phoneAliases)
	if phoneIdx == -1 {
		return nil, fmt.Errorf("missing phone column (searched for: %v)", phoneAliases)
	}
	indices["phone"] = phoneIdx

	// 2. Order Value
	var valAliases []string
	switch platform {
	case "shopify":
		valAliases = []string{"total", "total price", "subtotal"}
	case "woocommerce":
		valAliases = []string{"order total", "total", "amount", "order_total"}
	case "magento":
		valAliases = []string{"grand total", "total", "grand_total"}
	default:
		valAliases = []string{"order_value", "amount", "total", "value", "order value"}
	}
	valIdx := findMatch(valAliases)
	if valIdx == -1 {
		return nil, fmt.Errorf("missing order value column (searched for: %v)", valAliases)
	}
	indices["order_value"] = valIdx

	// 3. Order Date
	var dateAliases []string
	switch platform {
	case "shopify":
		dateAliases = []string{"created at", "created_at"}
	case "woocommerce":
		dateAliases = []string{"order date", "date", "order_date"}
	case "magento":
		dateAliases = []string{"created at", "created_at", "order date"}
	default:
		dateAliases = []string{"order_date", "date"}
	}
	dateIdx := findMatch(dateAliases)
	if dateIdx == -1 {
		return nil, fmt.Errorf("missing order date column (searched for: %v)", dateAliases)
	}
	indices["order_date"] = dateIdx

	// 4. Order ID
	var idAliases []string
	switch platform {
	case "shopify":
		idAliases = []string{"name", "order id", "id"}
	case "woocommerce":
		idAliases = []string{"order number", "order_number", "order id", "id"}
	case "magento":
		idAliases = []string{"increment id", "increment_id", "id", "order id"}
	default:
		idAliases = []string{"order_id", "id", "order_number"}
	}
	idIdx := findMatch(idAliases)
	if idIdx == -1 {
		return nil, fmt.Errorf("missing order ID column (searched for: %v)", idAliases)
	}
	indices["order_id"] = idIdx

	// 5. Order Status
	var statusAliases []string
	switch platform {
	case "shopify":
		statusAliases = []string{"fulfillment status", "fulfillment_status"}
	case "woocommerce":
		statusAliases = []string{"order status", "status", "order_status"}
	case "magento":
		statusAliases = []string{"status", "order status"}
	default:
		statusAliases = []string{"order_status", "status"}
	}
	statusIdx := findMatch(statusAliases)
	if statusIdx == -1 {
		return nil, fmt.Errorf("missing order status column (searched for: %v)", statusAliases)
	}
	indices["order_status"] = statusIdx

	// 6. Financial Status (Shopify only)
	if platform == "shopify" {
		finAliases := []string{"financial status", "financial_status"}
		finIdx := findMatch(finAliases)
		if finIdx == -1 {
			return nil, fmt.Errorf("missing financial status column for shopify (searched for: %v)", finAliases)
		}
		indices["financial_status"] = finIdx
	}

	return indices, nil
}
