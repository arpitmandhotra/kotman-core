package backfill

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/woocommerce"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TokenBucket implements a thread-safe token bucket rate limiter.
type TokenBucket struct {
	rate       float64
	capacity   float64
	tokens     float64
	lastRefill time.Time
	mu         sync.Mutex
}

func NewTokenBucket(rate, capacity float64) *TokenBucket {
	return &TokenBucket{
		rate:       rate,
		capacity:   capacity,
		tokens:     capacity,
		lastRefill: time.Now(),
	}
}

func (tb *TokenBucket) Wait(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(tb.lastRefill).Seconds()
		tb.lastRefill = now
		tb.tokens += elapsed * tb.rate
		if tb.tokens > tb.capacity {
			tb.tokens = tb.capacity
		}

		if tb.tokens >= 1.0 {
			tb.tokens -= 1.0
			tb.mu.Unlock()
			return nil
		}
		tb.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// backfillConcurrency limits the maximum number of concurrent backfills.
// Capped at 5, which is exactly 20% of MaxOpenConns (25) configured in internal/database/postgres.go.
// This prevents database connection starvation during heavy merchant onboarding.
var backfillConcurrency = make(chan struct{}, 5)

var backfillHttpClient = &http.Client{
	Timeout: 20 * time.Second,
}

// parseNextPageURL extracts the next page link from Shopify's Link header.
func parseNextPageURL(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	links := strings.Split(linkHeader, ",")
	for _, link := range links {
		if strings.Contains(link, `rel="next"`) {
			start := strings.Index(link, "<")
			end := strings.Index(link, ">")
			if start != -1 && end != -1 {
				return link[start+1 : end]
			}
		}
	}
	return ""
}

// BackfillOrderHistory runs the historical order backfill for a merchant.
func BackfillOrderHistory(ctx context.Context, merchantID, platform string) error {
	pg := database.NewPostgresClient()

	if platform == "shopify" {
		return RunShopifyBackfill(ctx, merchantID)
	}

	if platform == "woocommerce" {
		rdb := database.NewRedisClient()
		defer rdb.Close()
		return RunWooCommerceBackfill(ctx, merchantID, pg, rdb)
	}

	if platform == "magento" {
		var cred domain.PlatformCredential
		if err := pg.Where("merchant_id = ? AND platform = ? AND is_active = ?", merchantID, platform, true).First(&cred).Error; err != nil {
			return fmt.Errorf("failed to fetch platform credentials: %w", err)
		}

		totalProcessed := 0
		uniqueHashes := make(map[string]bool)
		preExistingRiskCount := 0
		var validPhoneCount, rejectedPhoneCount, rtoCount, deliveredCount int

		token, err := crypto.DecryptToken(cred.IntegrationTokenEncrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt integration token: %w", err)
		}

		page := 1
		pageSize := 100
		hasMore := true

		for hasMore {
			reqURL := fmt.Sprintf("%s/rest/V1/orders?searchCriteria[pageSize]=%d&searchCriteria[currentPage]=%d", cred.ShopDomain, pageSize, page)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return fmt.Errorf("failed to build Magento request: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			resp, err := backfillHttpClient.Do(req)
			if err != nil {
				return fmt.Errorf("Magento API call failed: %w", err)
			}

			var payload struct {
				Items []struct {
					EntityID       int64  `json:"entity_id"`
					Status         string `json:"status"`
					BillingAddress *struct {
						Telephone string `json:"telephone"`
					} `json:"billing_address"`
					ShippingAddress *struct {
						Telephone string `json:"telephone"`
					} `json:"shipping_address"`
				} `json:"items"`
				TotalCount int `json:"total_count"`
			}

			err = json.NewDecoder(io.LimitReader(resp.Body, 5<<20)).Decode(&payload)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("failed to decode Magento response: %w", err)
			}

			if len(payload.Items) == 0 {
				break
			}

			for _, order := range payload.Items {
				phone := ""
				if order.BillingAddress != nil && order.BillingAddress.Telephone != "" {
					phone = order.BillingAddress.Telephone
				} else if order.ShippingAddress != nil && order.ShippingAddress.Telephone != "" {
					phone = order.ShippingAddress.Telephone
				}

				if phone == "" {
					continue
				}

				hash := crypto.HashPhone(phone)
				uniqueHashes[hash] = true
				orderIDStr := fmt.Sprintf("%d", order.EntityID)

				var existing domain.BackfilledOrder
				err := pg.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error
				if err == nil {
					continue
				}

				pg.Create(&domain.BackfilledOrder{
					MerchantID: merchantID,
					Platform:   "magento",
					OrderID:    orderIDStr,
				})

				database.IncrementMetric(pg, hash, "total_orders")
				if order.Status == "complete" {
					database.IncrementMetric(pg, hash, "successful_deliveries")
				}
				if order.Status == "canceled" || order.Status == "closed" {
					database.IncrementMetric(pg, hash, "total_rtos")
				}

				totalProcessed++
				if totalProcessed%500 == 0 {
					slog.Info("backfill progress", "merchant_id", merchantID, "processed", totalProcessed, "platform", platform)
				}
			}

			if page*pageSize >= payload.TotalCount {
				hasMore = false
			} else {
				page++
			}
		}

		for hash := range uniqueHashes {
			var profile domain.TrustProfile
			if err := pg.Where("phone_hash = ?", hash).First(&profile).Error; err == nil {
				if profile.TotalRTOs > 2 {
					preExistingRiskCount++
				}
			}
		}

		slog.Info("historical order backfill completed summary",
			"merchant_id", merchantID,
			"platform", platform,
			"total_orders_processed", totalProcessed,
			"orders_with_valid_phone", validPhoneCount,
			"orders_phone_rejected", rejectedPhoneCount,
			"orders_flagged_rto", rtoCount,
			"orders_delivered", deliveredCount,
			"total_unique_hashes_created", len(uniqueHashes),
			"flagged_pre_existing_risk", preExistingRiskCount,
		)
		return nil
	}

	return fmt.Errorf("unsupported platform: %s", platform)
}

func backfillStartDate(storeCreatedAt time.Time) time.Time {
	horizon := time.Now().UTC().AddDate(0, -domain.ORDER_BACKFILL_MONTHS, 0)
	if storeCreatedAt.After(horizon) {
		return storeCreatedAt
	}
	return horizon
}

func RunShopifyBackfill(ctx context.Context, merchantID string) error {
	pg := database.NewPostgresClient()
	var merchant domain.Merchant
	if err := pg.First(&merchant, "id = ?", merchantID).Error; err != nil {
		return err
	}

	var storeCreatedAt time.Time
	if merchant.ShopifyCreatedAt != nil {
		storeCreatedAt = *merchant.ShopifyCreatedAt
	} else {
		storeCreatedAt = time.Now().UTC().AddDate(0, -domain.ORDER_BACKFILL_MONTHS, 0)
	}

	startDate := backfillStartDate(storeCreatedAt)

	// Mark as pending backfill
	now := time.Now().UTC()
	err := pg.Model(&merchant).Updates(map[string]interface{}{
		"backfill_status":     domain.BackfillPending,
		"backfill_started_at": &now,
		"backfill_horizon_at": startDate,
	}).Error
	if err != nil {
		return err
	}

	// Step 1: Submit bulk operation
	bulkOpID, err := submitShopifyBulkOperation(ctx, pg, &merchant, startDate)
	if err != nil {
		nowErr := time.Now().UTC()
		pg.Model(&merchant).Updates(map[string]interface{}{
			"backfill_status":        domain.BackfillFailed,
			"backfill_error_message": err.Error(),
			"backfill_completed_at":  &nowErr,
		})
		return err
	}

	// Step 2: Persist bulk operation state
	err = pg.Create(&domain.ShopifyBulkOperation{
		ID:              uuid.New(),
		MerchantID:      uuid.MustParse(merchant.ID),
		BulkOperationID: bulkOpID,
		Status:          "pending",
		SubmittedAt:     now,
	}).Error
	if err != nil {
		return err
	}

	return nil
}

func submitShopifyBulkOperation(ctx context.Context, pg *gorm.DB, merchant *domain.Merchant, startDate time.Time) (string, error) {
	var cred domain.PlatformCredential
	if err := pg.Where("merchant_id = ? AND platform = ? AND is_active = ?", merchant.ID, "shopify", true).First(&cred).Error; err != nil {
		return "", fmt.Errorf("failed to fetch platform credentials: %w", err)
	}

	token, err := crypto.DecryptToken(cred.AccessTokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt access token: %w", err)
	}

	startDateStr := startDate.Format(time.RFC3339)

	queryTemplate := `{
  orders(query: "created_at:>={start_date}") {
    edges {
      node {
        id
        name
        createdAt
        financialStatus
        fulfillmentStatus
        totalPriceSet { shopMoney { amount currencyCode } }
        customer { phone email firstName lastName }
        shippingAddress { zip city province country }
        lineItems {
          edges {
            node {
              title
              sku
              variant { id price product { id productType tags } }
              quantity
            }
          }
        }
      }
    }
  }
}`
	queryStr := strings.Replace(queryTemplate, "{start_date}", startDateStr, -1)

	type GraphQLRequest struct {
		Query string `json:"query"`
	}
	reqPayload := GraphQLRequest{
		Query: fmt.Sprintf(`mutation {
  bulkOperationRunQuery(
    query: """%s"""
  ) {
    bulkOperation {
      id
      status
    }
    userErrors {
      field
      message
    }
  }
}`, queryStr),
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return "", err
	}

	reqURL := fmt.Sprintf("https://%s/admin/api/2024-01/graphql.json", cred.ShopDomain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Shopify-Access-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := backfillHttpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("shopify graphql returned status %s", resp.Status)
	}

	var respPayload struct {
		Data struct {
			BulkOperationRunQuery struct {
				BulkOperation *struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"bulkOperation"`
				UserErrors []struct {
					Field   []string `json:"field"`
					Message string   `json:"message"`
				} `json:"userErrors"`
			} `json:"bulkOperationRunQuery"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&respPayload); err != nil {
		return "", err
	}

	if len(respPayload.Data.BulkOperationRunQuery.UserErrors) > 0 {
		var errMsgs []string
		for _, e := range respPayload.Data.BulkOperationRunQuery.UserErrors {
			errMsgs = append(errMsgs, e.Message)
		}
		return "", fmt.Errorf("shopify user errors: %s", strings.Join(errMsgs, "; "))
	}

	op := respPayload.Data.BulkOperationRunQuery.BulkOperation
	if op == nil {
		return "", fmt.Errorf("no bulkOperation returned from Shopify")
	}

	return op.ID, nil
}

func StartPoller(ctx context.Context, db *gorm.DB, rdb *redis.Client) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Info("running Shopify bulk operation poller iteration...")
			if err := pollPendingBulkOperations(ctx, db, rdb); err != nil {
				slog.Error("failed during Shopify bulk operation polling", "error", err)
			}
		}
	}
}

func pollPendingBulkOperations(ctx context.Context, db *gorm.DB, rdb *redis.Client) error {
	var ops []domain.ShopifyBulkOperation
	err := db.Where("status = ? OR status = ?", "pending", "running").Find(&ops).Error
	if err != nil {
		return err
	}

	for _, op := range ops {
		var cred domain.PlatformCredential
		if err := db.Where("merchant_id = ? AND platform = ? AND is_active = ?", op.MerchantID, "shopify", true).First(&cred).Error; err != nil {
			slog.Error("poller: failed to fetch platform credentials", "merchant_id", op.MerchantID, "error", err)
			continue
		}

		token, err := crypto.DecryptToken(cred.AccessTokenEncrypted)
		if err != nil {
			slog.Error("poller: failed to decrypt token", "merchant_id", op.MerchantID, "error", err)
			continue
		}

		query := fmt.Sprintf(`{
  node(id: "%s") {
    ... on BulkOperation {
      status
      url
      objectCount
    }
  }
}`, op.BulkOperationID)

		type GraphQLRequest struct {
			Query string `json:"query"`
		}
		reqPayload := GraphQLRequest{Query: query}
		reqBody, err := json.Marshal(reqPayload)
		if err != nil {
			continue
		}

		reqURL := fmt.Sprintf("https://%s/admin/api/2024-01/graphql.json", cred.ShopDomain)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
		if err != nil {
			continue
		}
		req.Header.Set("X-Shopify-Access-Token", token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := backfillHttpClient.Do(req)
		if err != nil {
			slog.Error("poller: query failed", "operation_id", op.BulkOperationID, "error", err)
			continue
		}

		var respPayload struct {
			Data struct {
				Node *struct {
					Status      string  `json:"status"`
					URL         *string `json:"url"`
					ObjectCount string  `json:"objectCount"`
				} `json:"node"`
			} `json:"data"`
		}

		decodeErr := json.NewDecoder(resp.Body).Decode(&respPayload)
		resp.Body.Close()
		if decodeErr != nil {
			continue
		}

		node := respPayload.Data.Node
		if node == nil {
			slog.Warn("poller: node not found for bulk operation", "id", op.BulkOperationID)
			continue
		}

		slog.Info("poller: bulk operation status update", "id", op.BulkOperationID, "status", node.Status, "object_count", node.ObjectCount)

		objCount, _ := strconv.Atoi(node.ObjectCount)
		op.ObjectCount = objCount

		switch node.Status {
		case "COMPLETED":
			if node.URL != nil {
				op.DownloadURL = *node.URL
				op.Status = "completed"
				now := time.Now().UTC()
				op.CompletedAt = &now
				db.Save(&op)

				db.Model(&domain.Merchant{}).Where("id = ?", op.MerchantID).Update("backfill_status", domain.BackfillInProgress)

				go func(opCopy domain.ShopifyBulkOperation) {
					processCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
					defer cancel()
					if err := downloadAndProcessBulkOp(processCtx, db, rdb, &opCopy); err != nil {
						slog.Error("failed to process bulk operation JSONL", "merchant_id", opCopy.MerchantID, "error", err)
						nowErr := time.Now().UTC()
						db.Model(&domain.Merchant{}).Where("id = ?", opCopy.MerchantID).Updates(map[string]interface{}{
							"backfill_status":        domain.BackfillFailed,
							"backfill_error_message": err.Error(),
							"backfill_completed_at":  &nowErr,
						})
					}
				}(op)
			} else {
				op.Status = "failed"
				op.ErrorMessage = "completed but URL is null"
				now := time.Now().UTC()
				op.CompletedAt = &now
				db.Save(&op)
				db.Model(&domain.Merchant{}).Where("id = ?", op.MerchantID).Updates(map[string]interface{}{
					"backfill_status":        domain.BackfillFailed,
					"backfill_error_message": op.ErrorMessage,
					"backfill_completed_at":  &now,
				})
			}

		case "FAILED", "CANCELED", "EXPIRED":
			op.Status = strings.ToLower(node.Status)
			op.ErrorMessage = fmt.Sprintf("bulk operation ended with status: %s", node.Status)
			now := time.Now().UTC()
			op.CompletedAt = &now
			db.Save(&op)
			db.Model(&domain.Merchant{}).Where("id = ?", op.MerchantID).Updates(map[string]interface{}{
				"backfill_status":        domain.BackfillFailed,
				"backfill_error_message": op.ErrorMessage,
				"backfill_completed_at":  &now,
			})

		default:
			op.Status = strings.ToLower(node.Status)
			db.Save(&op)
		}
	}

	return nil
}

type BulkLine struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	CreatedAt         string `json:"createdAt"`
	FinancialStatus   string `json:"financialStatus"`
	FulfillmentStatus string `json:"fulfillmentStatus"`
	TotalPriceSet     *struct {
		ShopMoney struct {
			Amount       string `json:"amount"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"shopMoney"`
	} `json:"totalPriceSet"`
	Customer *struct {
		Phone     string `json:"phone"`
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"customer"`
	ShippingAddress *struct {
		Zip      string `json:"zip"`
		City     string `json:"city"`
		Province string `json:"province"`
		Country  string `json:"country"`
	} `json:"shippingAddress"`
	__ParentID string `json:"__parentId"`
}

func downloadAndProcessBulkOp(ctx context.Context, db *gorm.DB, rdb *redis.Client, op *domain.ShopifyBulkOperation) error {
	resp, err := http.Get(op.DownloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download jsonl: status %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	totalProcessed := 0
	pincodeRepo := database.NewPincodeRepository(db, rdb)

	for scanner.Scan() {
		lineText := scanner.Text()
		if len(lineText) == 0 {
			continue
		}

		var line BulkLine
		if err := json.Unmarshal([]byte(lineText), &line); err != nil {
			continue
		}

		if line.__ParentID != "" {
			continue
		}

		parts := strings.Split(line.ID, "/")
		orderIDStr := parts[len(parts)-1]

		rawPhone := ""
		if line.Customer != nil && line.Customer.Phone != "" {
			rawPhone = line.Customer.Phone
		}

		cleanPhone, phoneValid := validateIndianMobilePhone(rawPhone)
		if !phoneValid {
			var existing domain.BackfilledOrder
			if db.Where("merchant_id = ? AND order_id = ?", op.MerchantID, orderIDStr).First(&existing).Error != nil {
				db.Create(&domain.BackfilledOrder{
					MerchantID: op.MerchantID.String(),
					Platform:   "shopify",
					OrderID:    orderIDStr,
				})
				totalProcessed++
			}
			continue
		}

		hash := crypto.HashPhone(cleanPhone)

		var existing domain.BackfilledOrder
		if db.Where("merchant_id = ? AND order_id = ?", op.MerchantID, orderIDStr).First(&existing).Error == nil {
			continue
		}

		db.Create(&domain.BackfilledOrder{
			MerchantID: op.MerchantID.String(),
			Platform:   "shopify",
			OrderID:    orderIDStr,
		})

		orderCreatedAt, _ := time.Parse(time.RFC3339, line.CreatedAt)
		
		isRTO := strings.ToLower(line.FinancialStatus) == "refunded" || strings.ToLower(line.FinancialStatus) == "voided"
		
		outcome := "DELIVERED"
		if isRTO {
			outcome = "RTO"
		} else if line.FulfillmentStatus != "fulfilled" {
			outcome = "PENDING"
		}

		orderValuePaise := 0
		if line.TotalPriceSet != nil {
			if val, err := strconv.ParseFloat(line.TotalPriceSet.ShopMoney.Amount, 64); err == nil {
				orderValuePaise = int(val * 100)
			}
		}

		paymentMethod := "prepaid"
		if strings.ToLower(line.FinancialStatus) == "pending" {
			paymentMethod = "cod"
		}

		pincode := ""
		city := ""
		state := ""
		if line.ShippingAddress != nil {
			pincode = line.ShippingAddress.Zip
			city = line.ShippingAddress.City
			state = line.ShippingAddress.Province
		}

		var geoState, geoTier, geoDistrict string
		var geoLat, geoLng float64
		geoTier = "TIER3"

		if pincode != "" {
			ref, err := pincodeRepo.GetPincodeInfo(ctx, pincode)
			if err == nil && ref != nil {
				geoState = ref.StateName
				geoTier = ref.GeoTier
				geoDistrict = ref.District
				geoLat = ref.Latitude
				geoLng = ref.Longitude
			}
		}

		orderUUID := uuid.NewSHA1(op.MerchantID, []byte(orderIDStr))

		orderRecord := domain.Order{
			ID:                     orderUUID,
			MerchantID:             op.MerchantID,
			OrderNumber:            line.Name,
			DeliveryStatus:         line.FulfillmentStatus,
			NDRAttempts:            0,
			CreatedAt:              orderCreatedAt,
			BuyerPhoneNormalized:   hash,
			BuyerEmail:             strings.ToLower(strings.TrimSpace(line.Customer.Email)),
			Outcome:                outcome,
			FulfillmentStatus:      line.FulfillmentStatus,
			PaymentMethod:          paymentMethod,
			OrderValuePaise:        orderValuePaise,
			ShippingAddressPincode: pincode,
			City:                   city,
			State:                  state,
			GeoState:               geoState,
			GeoTier:                geoTier,
			GeoDistrict:            geoDistrict,
			GeoLatitude:            geoLat,
			GeoLongitude:           geoLng,
		}

		db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"fulfillment_status", "financial_status", "outcome", "updated_at"}),
		}).Create(&orderRecord)

		database.IncrementMetric(db, hash, "total_orders")
		if line.FulfillmentStatus == "fulfilled" {
			database.IncrementMetric(db, hash, "successful_deliveries")
		}
		if isRTO {
			database.IncrementMetric(db, hash, "total_rtos")
		}

		totalProcessed++
		op.ProcessedCount = totalProcessed
		db.Save(op)

		if totalProcessed%500 == 0 {
			db.Model(&domain.Merchant{}).Where("id = ?", op.MerchantID.String()).Update("backfill_order_count", totalProcessed)
		}
	}

	now := time.Now().UTC()
	db.Model(&domain.Merchant{}).Where("id = ?", op.MerchantID.String()).Updates(map[string]interface{}{
		"backfill_status":      domain.BackfillComplete,
		"backfill_completed_at": &now,
		"backfill_order_count":  totalProcessed,
	})

	return scanner.Err()
}

type WooOrder struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	DateCreated   string `json:"date_created"`
	Total         string `json:"total"`
	PaymentMethod string `json:"payment_method"`
	Billing       struct {
		Phone string `json:"phone"`
		Email string `json:"email"`
	} `json:"billing"`
	Shipping struct {
		Phone    string `json:"phone"`
		City     string `json:"city"`
		State    string `json:"state"`
		Postcode string `json:"postcode"`
	} `json:"shipping"`
}

func RunWooCommerceBackfill(ctx context.Context, merchantID string, db *gorm.DB, rdb *redis.Client) error {
	var merchant domain.Merchant
	if err := db.First(&merchant, "id = ?", merchantID).Error; err != nil {
		return err
	}

	var cred domain.PlatformCredential
	if err := db.Where("merchant_id = ? AND platform = ? AND is_active = ?", merchantID, "woocommerce", true).First(&cred).Error; err != nil {
		return fmt.Errorf("failed to fetch WooCommerce credentials: %w", err)
	}

	key, err := crypto.DecryptToken(cred.ConsumerKeyEncrypted)
	if err != nil {
		return fmt.Errorf("failed to decrypt consumer key: %w", err)
	}
	secret, err := crypto.DecryptToken(cred.ConsumerSecretEncrypted)
	if err != nil {
		return fmt.Errorf("failed to decrypt consumer secret: %w", err)
	}

	startDate := backfillStartDate(merchant.CreatedAt)
	checkpointKey := fmt.Sprintf("woocommerce:backfill:checkpoint:%s", merchantID)

	if val, err := rdb.Get(ctx, checkpointKey).Result(); err == nil && val != "" {
		if parsedTime, err := time.Parse("2006-01-02T15:04:05", val); err == nil {
			startDate = parsedTime
		}
	}

	now := time.Now().UTC()
	db.Model(&merchant).Updates(map[string]interface{}{
		"backfill_status":     domain.BackfillInProgress,
		"backfill_started_at": &now,
		"backfill_horizon_at": startDate,
	})

	limiter := rate.NewLimiter(rate.Limit(2), 40)

	page := 1
	totalProcessed := merchant.BackfillOrderCount
	pincodeRepo := database.NewPincodeRepository(db, rdb)

	for {
		if err := limiter.Wait(ctx); err != nil {
			return err
		}

		afterStr := startDate.Format("2006-01-02T15:04:05")
		reqURL := fmt.Sprintf("%s/wp-json/wc/v3/orders?per_page=100&page=%d&order=asc&orderby=date&after=%s", cred.ShopDomain, page, afterStr)
		signedURL, err := woocommerce.SignRequest(http.MethodGet, reqURL, key, secret)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
		if err != nil {
			return err
		}

		resp, err := backfillHttpClient.Do(req)
		if err != nil {
			return err
		}

		var orders []WooOrder
		err = json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&orders)
		resp.Body.Close()
		if err != nil {
			return err
		}

		if len(orders) == 0 {
			break
		}

		for _, o := range orders {
			phone := o.Billing.Phone
			if phone == "" {
				phone = o.Shipping.Phone
			}

			cleanPhone, phoneValid := validateIndianMobilePhone(phone)
			if !phoneValid {
				orderIDStr := fmt.Sprintf("%d", o.ID)
				var existing domain.BackfilledOrder
				if db.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error != nil {
					db.Create(&domain.BackfilledOrder{
						MerchantID: merchantID,
						Platform:   "woocommerce",
						OrderID:    orderIDStr,
					})
					totalProcessed++
				}
				continue
			}

			hash := crypto.HashPhone(cleanPhone)
			orderIDStr := fmt.Sprintf("%d", o.ID)

			var existing domain.BackfilledOrder
			if db.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error == nil {
				continue
			}

			db.Create(&domain.BackfilledOrder{
				MerchantID: merchantID,
				Platform:   "woocommerce",
				OrderID:    orderIDStr,
			})

			orderCreatedAt, _ := time.Parse("2006-01-02T15:04:05", o.DateCreated)
			isRTO := o.Status == "refunded" || o.Status == "cancelled" || o.Status == "failed"

			outcome := "DELIVERED"
			if isRTO {
				outcome = "RTO"
			} else if o.Status != "completed" {
				outcome = "PENDING"
			}

			orderValuePaise := 0
			if val, err := strconv.ParseFloat(o.Total, 64); err == nil {
				orderValuePaise = int(val * 100)
			}

			paymentMethod := "prepaid"
			g := strings.ToLower(o.PaymentMethod)
			if g == "cod" || g == "cash_on_delivery" || g == "manual" || strings.Contains(g, "cod") {
				paymentMethod = "cod"
			}

			var geoState, geoTier, geoDistrict string
			var geoLat, geoLng float64
			geoTier = "TIER3"

			if o.Shipping.Postcode != "" {
				ref, err := pincodeRepo.GetPincodeInfo(ctx, o.Shipping.Postcode)
				if err == nil && ref != nil {
					geoState = ref.StateName
					geoTier = ref.GeoTier
					geoDistrict = ref.District
					geoLat = ref.Latitude
					geoLng = ref.Longitude
				}
			}

			merchantUUID, _ := uuid.Parse(merchantID)
			orderUUID := uuid.NewSHA1(merchantUUID, []byte(orderIDStr))

			orderRecord := domain.Order{
				ID:                     orderUUID,
				MerchantID:             merchantUUID,
				OrderNumber:            orderIDStr,
				DeliveryStatus:         o.Status,
				NDRAttempts:            0,
				CreatedAt:              orderCreatedAt,
				BuyerPhoneNormalized:   hash,
				BuyerEmail:             strings.ToLower(strings.TrimSpace(o.Billing.Email)),
				Outcome:                outcome,
				FulfillmentStatus:      o.Status,
				PaymentMethod:          paymentMethod,
				OrderValuePaise:        orderValuePaise,
				ShippingAddressPincode: o.Shipping.Postcode,
				City:                   o.Shipping.City,
				State:                  o.Shipping.State,
				GeoState:               geoState,
				GeoTier:                geoTier,
				GeoDistrict:            geoDistrict,
				GeoLatitude:            geoLat,
				GeoLongitude:           geoLng,
			}

			db.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoUpdates: clause.AssignmentColumns([]string{"fulfillment_status", "financial_status", "outcome", "updated_at"}),
			}).Create(&orderRecord)

			database.IncrementMetric(db, hash, "total_orders")
			if o.Status == "completed" {
				database.IncrementMetric(db, hash, "successful_deliveries")
			}
			if isRTO {
				database.IncrementMetric(db, hash, "total_rtos")
			}

			totalProcessed++
			rdb.Set(ctx, checkpointKey, o.DateCreated, 24*time.Hour)

			if totalProcessed%500 == 0 {
				db.Model(&merchant).Update("backfill_order_count", totalProcessed)
			}
		}

		page++
	}

	rdb.Del(ctx, checkpointKey)

	nowDone := time.Now().UTC()
	db.Model(&merchant).Updates(map[string]interface{}{
		"backfill_status":      domain.BackfillComplete,
		"backfill_completed_at": &nowDone,
		"backfill_order_count":  totalProcessed,
	})

	return nil
}
