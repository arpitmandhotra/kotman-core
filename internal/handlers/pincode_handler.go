package handlers

import (
	"strconv"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/gofiber/fiber/v2"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type PincodeHandler struct {
	repo *database.PincodeRepository
}

func NewPincodeHandler(db *gorm.DB, rdb *redis.Client) *PincodeHandler {
	return &PincodeHandler{
		repo: database.NewPincodeRepository(db, rdb),
	}
}

func (h *PincodeHandler) GetPincode(c *fiber.Ctx) error {
	pincode := c.Params("pincode")

	// Validate 6-digit numeric string
	if len(pincode) != 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "pincode must be a 6-digit string"})
	}
	if _, err := strconv.Atoi(pincode); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "pincode must be numeric"})
	}

	ctx := c.UserContext()
	ref, err := h.repo.GetPincodeInfo(ctx, pincode)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	if ref == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "pincode not found"})
	}

	// Cache-Control: public, max-age=86400
	c.Set("Cache-Control", "public, max-age=86400")

	return c.JSON(fiber.Map{
		"pincode":         ref.Pincode,
		"state":           ref.StateName,
		"district":        ref.District,
		"region":          ref.RegionName,
		"office_name":     ref.OfficeName,
		"geo_tier":        ref.GeoTier,
		"latitude":        ref.Latitude,
		"longitude":       ref.Longitude,
		"has_coordinates": ref.HasCoordinates,
	})
}
