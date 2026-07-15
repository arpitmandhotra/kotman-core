package handlers

import (
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type ScoreHandler struct {
	pg *gorm.DB
}

func NewScoreHandler(pgDB *gorm.DB) *ScoreHandler {
	return &ScoreHandler{pg: pgDB}
}

// GetMerchantScores returns all three scores (gating BUYER_QUALITY for free tier)
// Route: GET /v1/merchants/:id/scores
func (h *ScoreHandler) GetMerchantScores(c *fiber.Ctx) error {
	merchantID := c.Params("id")
	if merchantID == "" {
		return c.Status(400).JSON(fiber.Map{"error": "missing merchant id"})
	}

	ctx := c.UserContext()
	var merchant domain.Merchant
	if err := h.pg.WithContext(ctx).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "merchant not found"})
	}

	// Fetch current scores from DB
	var scores []domain.MerchantScore
	h.pg.WithContext(ctx).Where("merchant_id = ?", merchantID).Find(&scores)

	scoreMap := make(map[domain.ScoreType]*domain.MerchantScore)
	for i := range scores {
		scoreMap[scores[i].ScoreType] = &scores[i]
	}

	// Response structure
	response := fiber.Map{}

	// 1. OPERATIONS
	if ops, ok := scoreMap[domain.ScoreOperations]; ok {
		response["operations"] = fiber.Map{
			"score":       ops.Score,
			"computed_at": ops.ComputedAt,
			"valid_until": ops.ValidUntil,
		}
	} else {
		response["operations"] = fiber.Map{"status": "building..."}
	}

	// 2. RTO_EFFICIENCY
	if rto, ok := scoreMap[domain.ScoreRTOEfficiency]; ok {
		response["rto_efficiency"] = fiber.Map{
			"score":       rto.Score,
			"computed_at": rto.ComputedAt,
			"valid_until": rto.ValidUntil,
		}
	} else {
		response["rto_efficiency"] = fiber.Map{"status": "building..."}
	}

	// 3. BUYER_QUALITY (gated to Growth Tier)
	if !merchant.HasPaidSubscription {
		response["buyer_quality"] = domain.GatedScoreEnvelope{
			Gated:        true,
			TierRequired: "growth",
			Metric:       "buyer_quality_score",
		}
	} else {
		if bq, ok := scoreMap[domain.ScoreBuyerQuality]; ok {
			response["buyer_quality"] = fiber.Map{
				"score":       bq.Score,
				"computed_at": bq.ComputedAt,
				"valid_until": bq.ValidUntil,
			}
		} else {
			response["buyer_quality"] = fiber.Map{"status": "building..."}
		}
	}

	return c.JSON(response)
}

// GetMerchantScoreByType returns single score with full breakdown details
// Route: GET /v1/merchants/:id/scores/:type
func (h *ScoreHandler) GetMerchantScoreByType(c *fiber.Ctx) error {
	merchantID := c.Params("id")
	scoreTypeParam := domain.ScoreType(strings.ToUpper(c.Params("type")))

	if merchantID == "" || scoreTypeParam == "" {
		return c.Status(400).JSON(fiber.Map{"error": "missing parameters"})
	}

	ctx := c.UserContext()
	var merchant domain.Merchant
	if err := h.pg.WithContext(ctx).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "merchant not found"})
	}

	// Gate Buyer Quality Score to growth tier
	if scoreTypeParam == domain.ScoreBuyerQuality && !merchant.HasPaidSubscription {
		return c.Status(403).JSON(domain.GatedScoreEnvelope{
			Gated:        true,
			TierRequired: "growth",
			Metric:       "buyer_quality_score",
		})
	}

	var score domain.MerchantScore
	err := h.pg.WithContext(ctx).Preload("Breakdown").
		Where("merchant_id = ? AND score_type = ?", merchantID, string(scoreTypeParam)).
		First(&score).Error

	if err != nil {
		if gorm.ErrRecordNotFound == err {
			return c.JSON(fiber.Map{"status": "building...", "message": "score not yet computed"})
		}
		return c.Status(500).JSON(fiber.Map{"error": "database error"})
	}

	return c.JSON(score)
}

// GetMerchantScoreHistory returns historical score values
// Route: GET /v1/merchants/:id/scores/:type/history
func (h *ScoreHandler) GetMerchantScoreHistory(c *fiber.Ctx) error {
	merchantID := c.Params("id")
	scoreTypeParam := domain.ScoreType(strings.ToUpper(c.Params("type")))

	if merchantID == "" || scoreTypeParam == "" {
		return c.Status(400).JSON(fiber.Map{"error": "missing parameters"})
	}

	ctx := c.UserContext()
	var merchant domain.Merchant
	if err := h.pg.WithContext(ctx).Where("id = ?", merchantID).First(&merchant).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "merchant not found"})
	}

	// Gate Buyer Quality Score to growth tier
	if scoreTypeParam == domain.ScoreBuyerQuality && !merchant.HasPaidSubscription {
		return c.Status(403).JSON(domain.GatedScoreEnvelope{
			Gated:        true,
			TierRequired: "growth",
			Metric:       "buyer_quality_score",
		})
	}

	type HistoryPoint struct {
		Score      int       `json:"score"`
		ComputedAt time.Time `json:"computed_at"`
	}

	var history []HistoryPoint
	err := h.pg.WithContext(ctx).Model(&domain.MerchantScore{}).
		Select("score, computed_at").
		Where("merchant_id = ? AND score_type = ?", merchantID, string(scoreTypeParam)).
		Order("computed_at DESC").
		Limit(12).
		Scan(&history).Error

	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "database error"})
	}

	return c.JSON(history)
}
