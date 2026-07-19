package domain

import (
	"time"

	"gorm.io/gorm"
)

type ScoreType string

const (
	ScoreOperations    ScoreType = "OPERATIONS"
	ScoreRTOEfficiency ScoreType = "RTO_EFFICIENCY"
	ScoreBuyerQuality  ScoreType = "BUYER_QUALITY"
)

type DirectionType string

const (
	DirectionGood    DirectionType = "GOOD"
	DirectionNeutral DirectionType = "NEUTRAL"
	DirectionBad     DirectionType = "BAD"
)

type MerchantScore struct {
	gorm.Model
	MerchantID string           `gorm:"index;not null"`
	ScoreType  ScoreType        `gorm:"index;not null"`
	Score      int              `gorm:"not null"`
	ComputedAt time.Time        `gorm:"not null"`
	ValidUntil time.Time        `gorm:"not null"`
	Breakdown  []ScoreComponent `gorm:"foreignKey:MerchantScoreID;constraint:OnDelete:CASCADE"`
}

type ScoreComponent struct {
	gorm.Model
	MerchantScoreID uint          `gorm:"index;not null"`
	Name            string        `gorm:"not null"`
	Weight          float64       `gorm:"not null"`
	RawValue        float64       `gorm:"not null"`
	NormalizedScore int           `gorm:"not null"`
	Direction       DirectionType `gorm:"not null"`
	Description     string        `gorm:"type:text;not null"`
}

// GatedScoreEnvelope represents the upgrade prompt response envelope for paywalled scores
type GatedScoreEnvelope struct {
	Gated        bool   `json:"gated"`
	TierRequired string `json:"tier_required"`
	Metric       string `json:"metric"`
}

type AIScoreInsight struct {
	gorm.Model
	MerchantID  string    `gorm:"index;not null"`
	ScoreType   ScoreType `gorm:"not null"`
	ScoreValue  int       `gorm:"not null"`           // the score value at time of generation
	Insight     string    `gorm:"type:text;not null"` // AI-generated 2-sentence recommendation
	GeneratedAt time.Time `gorm:"not null"`
	ModelUsed   string    `gorm:"not null;default:'claude-sonnet-4-6'"`
}
