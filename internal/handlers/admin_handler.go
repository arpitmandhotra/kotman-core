package handlers

import (
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type AdminHandler struct {
	pg *gorm.DB
}

func NewAdminHandler(pg *gorm.DB) *AdminHandler {
	return &AdminHandler{pg: pg}
}

// GetRecentBlocks fetches the latest scammers caught by the Kotman engine
func (h *AdminHandler) GetRecentBlocks(c *fiber.Ctx) error {
	merchantName := "Admin" 
	var scammers []domain.BadActorRecord

	// Reach into Cold Storage and grab the 50 most recently caught scammers
	err := h.pg.Order("locked_at desc").Limit(50).Find(&scammers).Error
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"error":   "Failed to retrieve the vault data",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success":      true,
		"merchant":     merchantName,
		"total_blocks": len(scammers),
		"data":         scammers,
	})
}