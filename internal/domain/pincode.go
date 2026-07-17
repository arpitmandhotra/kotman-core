package domain

import "time"

type PincodeReference struct {
	// Primary identifier
	Pincode string `gorm:"primaryKey;type:char(6);not null"`

	// Geographic hierarchy
	StateName  string `gorm:"not null;index"`
	District   string `gorm:"not null;index"`
	RegionName string `gorm:"not null"` // postal region e.g. "Mumbai Region"
	CircleName string `gorm:"not null"` // postal circle e.g. "Maharashtra Circle"
	DivisionName string `gorm:"not null"`

	// Representative post office (pick the HEAD OFFICE row if available,
	// else the first DELIVERY post office, else any row for this pincode)
	OfficeName string `gorm:"not null"`
	OfficeType string `gorm:"not null"` // "HO" | "SO" | "BO"
	IsDelivery bool   `gorm:"not null;default:false"`

	// Coordinates (averaged across all post offices sharing this pincode)
	Latitude       float64 `gorm:"type:decimal(10,7)"`
	Longitude      float64 `gorm:"type:decimal(10,7)"`
	HasCoordinates bool    `gorm:"not null;default:false"` // false if lat/lng are 0 or invalid

	// Kaughtman-computed classification
	GeoTier string `gorm:"not null;default:'TIER3'"` // "METRO" | "TIER2" | "TIER3"

	// Metadata
	LastSyncedAt time.Time `gorm:"not null"`
}
