package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/woocommerce"
	"golang.org/x/time/rate"
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

	// Data quality counters (Shopify only — scoped inside the switch below)
	var (
		validPhoneCount    int
		rejectedPhoneCount int
		rtoCount           int
		deliveredCount     int

		// Fulfillment sync quality inputs
		ordersOlderThan45Days       int // denominator
		ordersWithFulfillmentStatus int // numerator
	)

	switch platform {
	case "shopify":
		token, err := crypto.DecryptToken(cred.AccessTokenEncrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt access token: %w", err)
		}

		// FIX 2: burst=40 matches Shopify's stated REST bucket; refills at 2 req/sec.
		limiter := rate.NewLimiter(rate.Limit(2), 40)

		// FIX 1: created_at_min prevents Shopify's silent 60-day truncation.
		cutoff := time.Now().UTC().AddDate(0, -18, 0)
		cutoffStr := cutoff.Format(time.RFC3339)
		nextURL := fmt.Sprintf(
			"https://%s/admin/api/2026-01/orders.json?status=any&limit=250&created_at_min=%s&order=created_at%%20asc",
			cred.ShopDomain,
			url.QueryEscape(cutoffStr),
		)

		// Sync quality gate: read the value computed from the PREVIOUS backfill run.
		// On first run this will be NULL → syncQualityTrusted = false → RTO proxy suppressed (safe default).
		// On retrigger runs, a computed value gates the proxy correctly for well-integrated merchants.
		const syncQualityThreshold = 0.60
		var merchantSyncQuality *float64
		var merchantForSync domain.Merchant
		if pg.Where("id = ?", merchantID).First(&merchantForSync).Error == nil {
			merchantSyncQuality = merchantForSync.FulfillmentSyncQuality
		}
		syncQualityTrusted := merchantSyncQuality != nil && *merchantSyncQuality >= syncQualityThreshold

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

			// FIX 2: 429 handling — read Shopify's Retry-After header.
			// continue retries the same nextURL (does NOT advance to next page).
			if resp.StatusCode == http.StatusTooManyRequests {
				retryAfter := resp.Header.Get("Retry-After")
				waitSeconds := 2.0
				if retryAfter != "" {
					if parsed, parseErr := strconv.ParseFloat(retryAfter, 64); parseErr == nil {
						waitSeconds = parsed
					}
				}
				resp.Body.Close()
				slog.Warn("shopify rate limit hit, backing off",
					"retry_after_seconds", waitSeconds,
					"merchant_id", merchantID,
				)
				time.Sleep(time.Duration(waitSeconds * float64(time.Second)))
				continue
			}

			var payload struct {
				Orders []struct {
					ID                int64     `json:"id"`
					FinancialStatus   string    `json:"financial_status"`
					FulfillmentStatus string    `json:"fulfillment_status"`
					CancelledAt       *string   `json:"cancelled_at"`
					CreatedAt         time.Time `json:"created_at"`
					Customer          *struct {
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

			err = json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&payload)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("failed to decode Shopify response: %w", err)
			}

			if len(payload.Orders) == 0 {
				break
			}

			for _, order := range payload.Orders {
				orderIDStr := fmt.Sprintf("%d", order.ID)

				// ── FULFILLMENT SYNC QUALITY INPUTS ────────────────────────────
				// Track independently of phone validity — all orders contribute.
				cutoff45 := time.Now().UTC().AddDate(0, 0, -45)
				if order.CreatedAt.UTC().Before(cutoff45) {
					ordersOlderThan45Days++
					if order.FulfillmentStatus != "" {
						ordersWithFulfillmentStatus++
					}
				}

				// ── PHONE EXTRACTION (unchanged priority logic) ─────────────────
				rawPhone := ""
				if order.Customer != nil && order.Customer.Phone != "" {
					rawPhone = order.Customer.Phone
				} else if order.BillingAddress != nil && order.BillingAddress.Phone != "" {
					rawPhone = order.BillingAddress.Phone
				} else if order.ShippingAddress != nil && order.ShippingAddress.Phone != "" {
					rawPhone = order.ShippingAddress.Phone
				}

				// ── PHONE VALIDATION GATE ───────────────────────────────────────
				// Every order counts for total_orders regardless of phone quality.
				// Only orders with valid phones enter the buyer trust network.
				cleanPhone, phoneValid := validateIndianMobilePhone(rawPhone)
				if !phoneValid {
					slog.Debug("phone rejected by validator, order counted but not network-linked",
						"order_id", order.ID,
						"merchant_id", merchantID,
						"raw_phone_length", len(rawPhone), // length only — never log the value
					)
					rejectedPhoneCount++
					// Still record the order in the idempotency table so retriggers
					// don't re-process it, but skip trust profile upsert.
					var existing domain.BackfilledOrder
					if pg.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error != nil {
						pg.Create(&domain.BackfilledOrder{
							MerchantID: merchantID,
							Platform:   "shopify",
							OrderID:    orderIDStr,
						})
						totalProcessed++
					}
					continue // skip hash + trust score upsert
				}
				validPhoneCount++

				hash := crypto.HashPhone(cleanPhone)
				uniqueHashes[hash] = true

				// Idempotency: skip if already processed
				var existing domain.BackfilledOrder
				if pg.Where("merchant_id = ? AND order_id = ?", merchantID, orderIDStr).First(&existing).Error == nil {
					continue // already backfilled
				}

				// Record processed order
				pg.Create(&domain.BackfilledOrder{
					MerchantID: merchantID,
					Platform:   "shopify",
					OrderID:    orderIDStr,
				})

				// ── RTO DETECTION ──────────────────────────────────────────────
				// The unfulfilled-paid proxy is gated on syncQualityTrusted.
				// First run: NULL quality → proxy suppressed (conservative, safe).
				// Retrigger: computed quality gates the proxy for calibrated accuracy.
				orderAge := time.Since(order.CreatedAt.UTC())
				isUnfulfilledPaidRTO := syncQualityTrusted &&
					order.FulfillmentStatus == "" &&
					order.FinancialStatus == "paid" &&
					orderAge > 30*24*time.Hour

				if isUnfulfilledPaidRTO {
					slog.Debug("flagging unfulfilled paid order as RTO proxy",
						"order_id", order.ID,
						"merchant_id", merchantID,
						"order_age_days", int(orderAge.Hours()/24),
					)
				}

				isRTO := order.FinancialStatus == "refunded" ||
					order.FinancialStatus == "voided" ||
					order.CancelledAt != nil ||
					isUnfulfilledPaidRTO

				// Increment trust metrics
				database.IncrementMetric(pg, hash, "total_orders")
				if order.FulfillmentStatus == "fulfilled" {
					database.IncrementMetric(pg, hash, "successful_deliveries")
					deliveredCount++
				}
				if isRTO {
					database.IncrementMetric(pg, hash, "total_rtos")
					rtoCount++
				}

				totalProcessed++
				if totalProcessed%500 == 0 {
					slog.Info("backfill progress", "merchant_id", merchantID, "processed", totalProcessed, "platform", platform)
				}
			}

			nextURL = parseNextPageURL(resp.Header.Get("Link"))
		}

		// ── FULFILLMENT SYNC QUALITY WRITE ──────────────────────────────────
		// Computed AFTER all pages are processed — we need the full picture.
		// This value is used by the RTO proxy gate on the NEXT retrigger run.
		if ordersOlderThan45Days >= 50 {
			syncQuality := float64(ordersWithFulfillmentStatus) / float64(ordersOlderThan45Days)
			now := time.Now().UTC()
			if err := pg.Model(&domain.Merchant{}).Where("id = ?", merchantID).Updates(map[string]interface{}{
				"fulfillment_sync_quality":     syncQuality,
				"fulfillment_sync_computed_at": now,
			}).Error; err != nil {
				// Non-fatal: log and continue — do not fail the whole backfill.
				slog.Error("failed to update fulfillment sync quality",
					"merchant_id", merchantID,
					"error", err,
				)
			} else {
				slog.Info("fulfillment sync quality computed",
					"merchant_id", merchantID,
					"sync_quality", fmt.Sprintf("%.4f", syncQuality),
					"orders_sampled", ordersOlderThan45Days,
					"synced_count", ordersWithFulfillmentStatus,
					"rto_proxy_was_active", syncQualityTrusted,
				)
			}
		} else {
			slog.Info("fulfillment sync quality not computed — insufficient sample",
				"merchant_id", merchantID,
				"orders_older_than_45d", ordersOlderThan45Days,
			)
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
		"orders_with_valid_phone", validPhoneCount,
		"orders_phone_rejected", rejectedPhoneCount,
		"orders_flagged_rto", rtoCount,
		"orders_delivered", deliveredCount,
		"total_unique_hashes_created", len(uniqueHashes),
		"flagged_pre_existing_risk", preExistingRiskCount,
	)

	return nil
}
