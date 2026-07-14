package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CatalogSyncService struct {
	pg         *gorm.DB
	httpClient *http.Client
}

func NewCatalogSyncService(pgDB *gorm.DB) *CatalogSyncService {
	return &CatalogSyncService{
		pg: pgDB,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ShopifyProductResponse models the top-level Shopify REST API product list response.
type ShopifyProductResponse struct {
	Products []ShopifyProduct `json:"products"`
}

type ShopifyProduct struct {
	ID         int64            `json:"id"`
	Title      string           `json:"title"`
	ProductType string          `json:"product_type"`
	Tags       string           `json:"tags"`
	Variants   []ShopifyVariant `json:"variants"`
}

type ShopifyVariant struct {
	ID        int64  `json:"id"`
	ProductID int64  `json:"product_id"`
	Title     string `json:"title"`
	SKU       string `json:"sku"`
	Price     string `json:"price"`
	CompareAt string `json:"compare_at_price"`
}

// SyncShopifyCatalog performs the full initial backfill of products from Shopify REST API.
// Handles leaky-bucket rate limits and follows RFC 5988 Link headers for pagination.
func (s *CatalogSyncService) SyncShopifyCatalog(ctx context.Context, merchantID string, shopURL string, accessToken string) error {
	slog.Info("starting Shopify product catalog backfill sync", "merchant_id", merchantID, "shop_url", shopURL)

	// API URL for initial page
	nextPageURL := fmt.Sprintf("https://%s/admin/api/2024-01/products.json?limit=50", shopURL)

	for nextPageURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextPageURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create sync request: %w", err)
		}
		req.Header.Set("X-Shopify-Access-Token", accessToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to execute catalog API request: %w", err)
		}

		// Handle Shopify leaky-bucket rate limiting
		s.handleRateLimiting(resp)

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return fmt.Errorf("shopify API returned error status: %d, body: %s", resp.StatusCode, string(body))
		}

		var payload ShopifyProductResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to decode shopify product payload: %w", err)
		}
		resp.Body.Close()

		// Upsert products to database
		if len(payload.Products) > 0 {
			err = s.saveShopifyProducts(ctx, merchantID, payload.Products)
			if err != nil {
				return fmt.Errorf("failed to save products to catalog: %w", err)
			}
		}

		// Parse next page link from standard Link header
		nextPageURL = getNextPageLink(resp.Header.Get("Link"))
	}

	slog.Info("completed Shopify catalog sync successfully", "merchant_id", merchantID)
	return nil
}

func (s *CatalogSyncService) handleRateLimiting(resp *http.Response) {
	// Header: X-Shopify-Shop-Api-Call-Limit: 10/40
	limitHeader := resp.Header.Get("X-Shopify-Shop-Api-Call-Limit")
	if limitHeader == "" {
		return
	}

	parts := strings.Split(limitHeader, "/")
	if len(parts) != 2 {
		return
	}

	used, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	total, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return
	}

	// If 85%+ of bucket capacity is used, pause to allow cool down
	usagePercent := float64(used) / float64(total)
	if usagePercent > 0.85 {
		sleepDuration := 2 * time.Second
		slog.Warn("shopify rate limit warning: leaky bucket high utilization, slowing down",
			"limit", limitHeader, "sleep_seconds", sleepDuration.Seconds())
		time.Sleep(sleepDuration)
	}
}

func (s *CatalogSyncService) saveShopifyProducts(ctx context.Context, merchantID string, products []ShopifyProduct) error {
	return s.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var catalogItems []domain.ProductCatalog

		for _, p := range products {
			for _, v := range p.Variants {
				pricePaise := parsePriceToPaise(v.Price)
				comparePaise := parsePriceToPaise(v.CompareAt)

				catalogItems = append(catalogItems, domain.ProductCatalog{
					MerchantID:     merchantID,
					ProductID:      strconv.FormatInt(p.ID, 10),
					VariantID:      strconv.FormatInt(v.ID, 10),
					Title:          fmt.Sprintf("%s - %s", p.Title, v.Title),
					SKU:            v.SKU,
					Category:       p.ProductType,
					Tags:           p.Tags,
					PricePaise:     pricePaise,
					CompareAtPaise: comparePaise,
					LastSyncedAt:   time.Now(),
				})
			}
		}

		// Perform bulk upsert (insert on conflict update variant details)
		if len(catalogItems) > 0 {
			err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "variant_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"title", "sku", "category", "tags", "price_paise", "compare_at_paise", "last_synced_at"}),
			}).Create(&catalogItems).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// getNextPageLink parses standard HTTP Link header according to RFC 5988:
// Link: <https://shop.myshopify.com/admin/api/2024-01/products.json?limit=50&page_info=xyz>; rel="next"
func getNextPageLink(header string) string {
	if header == "" {
		return ""
	}

	links := strings.Split(header, ",")
	for _, link := range links {
		parts := strings.Split(strings.TrimSpace(link), ";")
		if len(parts) < 2 {
			continue
		}

		urlPart := parts[0]
		relPart := parts[1]

		if strings.Contains(relPart, `rel="next"`) {
			urlPart = strings.TrimLeft(urlPart, "<")
			urlPart = strings.TrimRight(urlPart, ">")
			return urlPart
		}
	}
	return ""
}

func parsePriceToPaise(priceStr string) int {
	if priceStr == "" {
		return 0
	}
	var f float64
	_, err := fmt.Sscanf(priceStr, "%f", &f)
	if err != nil {
		return 0
	}
	return int(f * 100) // Convert rupees to paise
}
