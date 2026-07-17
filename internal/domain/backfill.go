package domain

import (
	"time"

	"github.com/google/uuid"
)

type ShopifyBulkOperation struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	MerchantID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"merchant_id"`
	BulkOperationID string     `gorm:"not null" json:"bulk_operation_id"` // Shopify's gid://shopify/BulkOperation/...
	Status          string     `gorm:"not null;default:'pending'" json:"status"` // pending | running | completed | failed
	ObjectCount     int        `gorm:"default:0" json:"object_count"` // how many orders Shopify says it found
	DownloadURL     string     `json:"download_url"`
	ProcessedCount  int        `gorm:"default:0" json:"processed_count"` // how many we've actually ingested
	SubmittedAt     time.Time  `json:"submitted_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	ErrorMessage    string     `json:"error_message"`
}
