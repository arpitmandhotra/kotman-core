package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/woocommerce"
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
	select {
	case backfillConcurrency <- struct{}{}:
		defer func() { <-backfillConcurrency }()
	default:
		slog.Info("backfill waiting for concurrency slot", "merchant_id", merchantID, "queue_depth", len(backfillConcurrency))
		select {
		case backfillConcurrency <- struct{}{}:
			defer func() { <-backfillConcurrency }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	slog.Info("starting historical order backfill", "merchant_id", merchantID, "platform", platform)

	pg := database.NewPostgresClient()

	// 1. Fetch credentials
	var cred domain.PlatformCredential
	if err := pg.Where("merchant_id = ? AND platform = ? AND is_active = ?", merchantID, platform, true).First(&cred).Error; err != nil {
		return fmt.Errorf("failed to fetch platform credentials: %w", err)
	}

	totalProcessed := 0
	uniqueHashes := make(map[string]bool)
	preExistingRiskCount := 0

	switch platform {
	case "shopify":
		token, err := crypto.DecryptToken(cred.AccessTokenEncrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt access token: %w", err)
		}

		// Implement Shopify rate limiter: 2 requests/sec
		limiter := NewTokenBucket(2.0, 40.0)

		nextURL := fmt.Sprintf("https://%s/admin/api/2026-01/orders.json?status=any&limit=250", cred.ShopDomain)

		for nextURL != "" {
			if err := limiter.Wait(ctx); err != nil {
				return err
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
			if err != nil {
				return fmt.Errorf("failed to build request: %w", err)
			}
			req.Header.Set("X-Shopify-Access-Token", token)

			resp, err := backfillHttpClient.Do(req)
			if err != nil {
				return fmt.Errorf("Shopify API call failed: %w", err)
			}

			var payload struct {
				Orders []struct {
					ID              int64   `json:"id"`
					FinancialStatus string  `json:"financial_status"`
					FulfillmentStatus string `json:"fulfillment_status"`
					CancelledAt     *string `json:"cancelled_at"`
					Customer        *struct {
						Phone string `json:"phone"`
					} `json:"customer"`
					BillingAddress *struct {
						Phone string `json:"phone"`
					} `json:"billing_address"`
					ShippingAddress *struct {
						Phone string `json:"phone"`
					} `json:"shipping_address"`
				} `json:"orders"`
			}

			err = json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&payload) // 10MB cap for batch
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("failed to decode Shopify response: %w", err)
			}

			if len(payload.Orders) == 0 {
				break
			}

			for _, order := range payload.Orders {
				phone := ""
				if order.Customer != nil && order.Customer.Phone != "" {
					phone = order.Customer.Phone
				} else if order.BillingAddress != nil && order.BillingAddress.Phone != "" {
					phone = order.BillingAddress.Phone
				} else if order.ShippingAddress != nil && order.ShippingAddress.Phone != "" {
					phone = order.ShippingAddress.Phone
				}

				if phone == "" {
					continue
				}

				hash := crypto.HashPhone(phone)
				uniqueHashes[hash] = true
				orderIDStr := fmt.Sprintf("%d", order.ID)

				// Idempotency: skip if already processed
				var existing domain.BackfilledOrder
				err := pg.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error
				if err == nil {
					continue // already backfilled
				}

				// Record processed order
				pg.Create(&domain.BackfilledOrder{
					MerchantID: merchantID,
					Platform:   "shopify",
					OrderID:    orderIDStr,
				})

				// Increment metrics
				database.IncrementMetric(pg, hash, "total_orders")
				if order.FulfillmentStatus == "fulfilled" {
					database.IncrementMetric(pg, hash, "successful_deliveries")
				}
				if order.CancelledAt != nil || order.FinancialStatus == "refunded" || order.FinancialStatus == "voided" {
					database.IncrementMetric(pg, hash, "total_rtos")
				}

				totalProcessed++
				if totalProcessed%500 == 0 {
					slog.Info("backfill progress", "merchant_id", merchantID, "processed", totalProcessed, "platform", platform)
				}
			}

			nextURL = parseNextPageURL(resp.Header.Get("Link"))
		}

	case "woocommerce":
		key, err := crypto.DecryptToken(cred.ConsumerKeyEncrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt consumer key: %w", err)
		}
		secret, err := crypto.DecryptToken(cred.ConsumerSecretEncrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt consumer secret: %w", err)
		}

		page := 1
		totalPages := 1

		for page <= totalPages {
			reqURL := fmt.Sprintf("%s/wp-json/wc/v3/orders?per_page=100&page=%d", cred.ShopDomain, page)
			signedURL, err := woocommerce.SignRequest(http.MethodGet, reqURL, key, secret)
			if err != nil {
				return fmt.Errorf("failed to sign WooCommerce request: %w", err)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
			if err != nil {
				return fmt.Errorf("failed to build WooCommerce request: %w", err)
			}

			resp, err := backfillHttpClient.Do(req)
			if err != nil {
				return fmt.Errorf("WooCommerce API call failed: %w", err)
			}

			// Parse total pages header
			if totalPagesStr := resp.Header.Get("X-WP-TotalPages"); totalPagesStr != "" {
				fmt.Sscanf(totalPagesStr, "%d", &totalPages)
			}

			var orders []struct {
				ID      int64  `json:"id"`
				Status  string `json:"status"`
				Billing struct {
					Phone string `json:"phone"`
				} `json:"billing"`
				Shipping struct {
					Phone string `json:"phone"`
				} `json:"shipping"`
			}

			err = json.NewDecoder(io.LimitReader(resp.Body, 5<<20)).Decode(&orders)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("failed to decode WooCommerce response: %w", err)
			}

			if len(orders) == 0 {
				break
			}

			for _, order := range orders {
				phone := order.Billing.Phone
				if phone == "" {
					phone = order.Shipping.Phone
				}
				if phone == "" {
					continue
				}

				hash := crypto.HashPhone(phone)
				uniqueHashes[hash] = true
				orderIDStr := fmt.Sprintf("%d", order.ID)

				var existing domain.BackfilledOrder
				err := pg.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error
				if err == nil {
					continue
				}

				pg.Create(&domain.BackfilledOrder{
					MerchantID: merchantID,
					Platform:   "woocommerce",
					OrderID:    orderIDStr,
				})

				database.IncrementMetric(pg, hash, "total_orders")
				if order.Status == "completed" {
					database.IncrementMetric(pg, hash, "successful_deliveries")
				}
				if order.Status == "refunded" || order.Status == "cancelled" {
					database.IncrementMetric(pg, hash, "total_rtos")
				}

				totalProcessed++
				if totalProcessed%500 == 0 {
					slog.Info("backfill progress", "merchant_id", merchantID, "processed", totalProcessed, "platform", platform)
				}
			}

			page++
		}

	case "magento":
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
	}

	// Calculate pre-existing risks
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
		"total_unique_hashes_created", len(uniqueHashes),
		"flagged_pre_existing_risk", preExistingRiskCount,
	)

	return nil
}
