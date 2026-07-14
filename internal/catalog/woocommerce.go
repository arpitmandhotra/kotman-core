package catalog

import (
	"context"

	"github.com/google/uuid"
)

type WooCommerceSyncClient struct{}

func NewWooCommerceSyncClient() *WooCommerceSyncClient {
	return &WooCommerceSyncClient{}
}

func (c *WooCommerceSyncClient) FetchAndSyncCatalog(ctx context.Context, merchantID uuid.UUID, storeURL string, consumerKey string, consumerSecret string) error {
	// TODO: implement WooCommerce OAuth 1.0a paginated GET /wp-json/wc/v3/products.
	return nil
}
