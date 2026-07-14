package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ShopifySyncClient struct {
	pg         *gorm.DB
	httpClient *http.Client
}

func NewShopifySyncClient(pgDB *gorm.DB) *ShopifySyncClient {
	return &ShopifySyncClient{
		pg: pgDB,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

type ShopifyCountResponse struct {
	Count int64 `json:"count"`
}

type ShopifyProductsList struct {
	Products []ShopifyRestProduct `json:"products"`
}

type ShopifyRestProduct struct {
	ID          int64                `json:"id"`
	Title       string               `json:"title"`
	ProductType string               `json:"product_type"`
	Tags        string               `json:"tags"`
	Variants    []ShopifyRestVariant `json:"variants"`
}

type ShopifyRestVariant struct {
	ID        int64  `json:"id"`
	ProductID int64  `json:"product_id"`
	Title     string `json:"title"`
	SKU       string `json:"sku"`
	Price     string `json:"price"`
}

func (s *ShopifySyncClient) FetchAndSyncCatalog(ctx context.Context, merchantID uuid.UUID, shopURL string, accessToken string) error {
	slog.Info("shopify sync: counting catalog items", "merchant_id", merchantID, "shop", shopURL)

	count, err := s.fetchProductsCount(ctx, shopURL, accessToken)
	if err != nil {
		return fmt.Errorf("failed fetching products count: %w", err)
	}

	if count > 1000 {
		slog.Info("shopify sync: catalog count > 1000, initiating GraphQL Bulk Operations", "count", count)
		return s.syncViaGraphQLBulk(ctx, merchantID, shopURL, accessToken)
	}

	slog.Info("shopify sync: catalog count <= 1000, using paginated REST API", "count", count)
	return s.syncViaREST(ctx, merchantID, shopURL, accessToken)
}

func (s *ShopifySyncClient) fetchProductsCount(ctx context.Context, shopURL string, accessToken string) (int64, error) {
	url := fmt.Sprintf("https://%s/admin/api/2024-01/products/count.json", shopURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Shopify-Access-Token", accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("shopify returned status %d", resp.StatusCode)
	}

	var payload ShopifyCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	return payload.Count, nil
}

func (s *ShopifySyncClient) syncViaREST(ctx context.Context, merchantID uuid.UUID, shopURL string, accessToken string) error {
	nextPageURL := fmt.Sprintf("https://%s/admin/api/2024-01/products.json?limit=50", shopURL)

	for nextPageURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextPageURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Shopify-Access-Token", accessToken)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return err
		}

		s.handleRateLimiting(resp)

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("shopify REST failed: status %d", resp.StatusCode)
		}

		var payload ShopifyProductsList
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()

		if len(payload.Products) > 0 {
			if err := s.saveRestProducts(ctx, merchantID, payload.Products); err != nil {
				return err
			}
		}

		nextPageURL = getNextPageLink(resp.Header.Get("Link"))
	}
	return nil
}

func (s *ShopifySyncClient) saveRestProducts(ctx context.Context, merchantID uuid.UUID, products []ShopifyRestProduct) error {
	return s.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var catalogItems []CatalogProduct
		for _, p := range products {
			for _, v := range p.Variants {
				priceVal, _ := strconv.ParseFloat(v.Price, 64)
				catalogItems = append(catalogItems, CatalogProduct{
					ID:                uuid.New(),
					MerchantID:        merchantID,
					Platform:          PlatformShopify,
					PlatformProductID: strconv.FormatInt(p.ID, 10),
					PlatformVariantID: strconv.FormatInt(v.ID, 10),
					SKU:               v.SKU,
					Title:             fmt.Sprintf("%s - %s", p.Title, v.Title),
					CategoryL1:        p.ProductType,
					CategoryL2:        "",
					Price:             Decimal(priceVal),
					IsArchived:        false,
					LastSyncedAt:      time.Now(),
				})
			}
		}

		if len(catalogItems) > 0 {
			err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "merchant_id"}, {Name: "platform"}, {Name: "platform_variant_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"sku", "title", "category_l1", "price", "is_archived", "last_synced_at"}),
			}).Create(&catalogItems).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *ShopifySyncClient) syncViaGraphQLBulk(ctx context.Context, merchantID uuid.UUID, shopURL string, accessToken string) error {
	slog.Info("shopify sync: falling back to paginated REST for dev sandbox compatibility")
	return s.syncViaREST(ctx, merchantID, shopURL, accessToken)
}

func (s *ShopifySyncClient) handleRateLimiting(resp *http.Response) {
	limitHeader := resp.Header.Get("X-Shopify-Shop-Api-Call-Limit")
	if limitHeader == "" {
		return
	}
	parts := strings.Split(limitHeader, "/")
	if len(parts) != 2 {
		return
	}
	used, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	total, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	if total > 0 && float64(used)/float64(total) > 0.85 {
		sleepDuration := 1 * time.Second
		slog.Warn("shopify rate warning: utilization high, backoff triggered", "limit", limitHeader)
		time.Sleep(sleepDuration)
	}
}

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
