package catalog

import (
	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

// Re-export type definitions to keep package structures clean
type PlatformType = domain.PlatformType

const (
	PlatformShopify     PlatformType = domain.PlatformShopify
	PlatformWooCommerce PlatformType = domain.PlatformWooCommerce
)

type Decimal = domain.Decimal
type CatalogProduct = domain.CatalogProduct
type Order = domain.Order
type OrderLineItem = domain.OrderLineItem
