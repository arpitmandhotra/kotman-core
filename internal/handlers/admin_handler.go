package handlers

import (
	"encoding/csv"
	"io"
	"log/slog"
      "crypto/rand"
	"encoding/hex"
	"github.com/arpitmandhotra/api-integrator/internal/domain"

	"github.com/gofiber/fiber/v2"
	// Adjust this import path to match your actual domain/crypto packages
	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AdminHandler struct {
	pg *gorm.DB
}

func NewAdminHandler(pg *gorm.DB) *AdminHandler {
	return &AdminHandler{pg: pg}
}

// ImportBadActorsCSV handles the multipart form upload
func (h *AdminHandler) ImportBadActorsCSV(c *fiber.Ctx) error {
	// 1. Grab the multipart file metadata from the request
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "missing 'file' in form-data",
		})
	}

	// 2. Open the file stream (This prevents loading the whole file into RAM)
	file, err := fileHeader.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to open file stream",
		})
	}
	defer file.Close() // ALWAYS defer the close to prevent memory leaks

	// 3. Initialize the CSV Reader
	reader := csv.NewReader(file)

	// 4. Read the header row to validate the merchant's format
	headers, err := reader.Read()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed to read csv headers or file is empty",
		})
	}

	// Strict format enforcement
	if len(headers) < 2 || headers[0] != "phone" || headers[1] != "reason" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid csv format. Expected headers: phone, reason",
		})
	}

	var batch []domain.TrustProfile
	const batchSize = 1000 // We will send to Postgres in blocks of 1,000
	totalProcessed := 0
	totalSkipped := 0

	// Get the merchant ID from the context (set by your RequireAdminKey auth middleware)
	merchantID, _ := c.Locals("merchant_id").(string)

	// 5. Stream the rows continuously
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break // End of file reached, exit the loop cleanly
		}
		if err != nil {
			slog.Warn("skipping malformed csv row", "error", err)
			totalSkipped++
			continue
		}

		rawPhone := row[0]
		reason := row[1]

		if rawPhone == "" {
			totalSkipped++
			continue
		}

		// 6. The Crypto Wiring - Hash before it ever touches your database
		phoneHash := crypto.HashPhone(rawPhone)

		// Append the cleaned data to our current batch
		// Append the cleaned data to our current batch
		batch = append(batch, domain.TrustProfile{
			PhoneHash:           phoneHash,
			FirstSeenMerchantID: merchantID, // Changed from MerchantID
			
			// --- System Overrides ---
			IsBlacklisted:   true,   // Explicitly mark as a bad actor
			BlacklistReason: reason, // Changed from Reason
			
			// 🧠 --- V2 AI Baseline Initialization --- 🧠
			// If they are on a merchant's blocklist CSV, we must assume 
			// they have historically ruined at least one order.
			TotalOrders: 1, 
			TotalRTOs:   1, 
		})

		// 7. Flush to Database when the batch hits 1,000 records
		if len(batch) >= batchSize {
			// Direct Gorm bulk insert ignoring duplicates
			if dbErr := h.pg.WithContext(c.Context()).Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error; dbErr != nil {
				slog.Error("failed to insert batch", "error", dbErr)
			} else {
				totalProcessed += len(batch)
			}

			// Clear the batch slice for the next 1,000 records, reusing the memory
			batch = batch[:0]
		}
	}

	// 8. Flush any leftover records (e.g., the final 342 records of a 5,342 row file)
	if len(batch) > 0 {
		if dbErr := h.pg.WithContext(c.Context()).Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error; dbErr != nil {
			slog.Error("failed to insert final batch", "error", dbErr)
		} else {
			totalProcessed += len(batch)
		}
	}

	// 9. Return the success telemetry to the frontend
	return c.JSON(fiber.Map{
		"message":           "import complete",
		"records_processed": totalProcessed,
		"records_skipped":   totalSkipped,
	})
}

// GetRecentBlocks fetches the latest scammers caught by the Kotman engine
func (h *AdminHandler) GetRecentBlocks(c *fiber.Ctx) error {
	merchantName := "Admin"
	var scammers []domain.TrustProfile

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
// OnboardMerchantRequest holds incoming data for creating a new Shopify client
type OnboardMerchantRequest struct {
	StoreName string `json:"store_name"`
}

// OnboardMerchant generates a secure API credential and inserts a new merchant profile using UUIDs
func (h *AdminHandler) OnboardMerchant(c *fiber.Ctx) error {
	var req OnboardMerchantRequest
	if err := c.BodyParser(&req); err != nil || req.StoreName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "A valid store_name string is required",
		})
	}

	// 1. Generate 32 bytes of cryptographically secure randomness
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to safely generate crypto random bytes",
		})
	}

	// 2. Format a clean token prefix for their storefront integration
	apiKey := "kt_live_" + hex.EncodeToString(bytes)

	// 3. Assemble the updated Merchant schema
	merchant := domain.Merchant{
		StoreName: req.StoreName,
		APIKey:    apiKey,
		IsActive:  true,
		// ID (UUID string format) and Timestamps are automatically handled by Postgres/Gorm definitions
	}

	// 4. Persistence execution
	if err := h.pg.Create(&merchant).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to commit new merchant credentials to database",
		})
	}

	// 5. Return payload so you can hand this key over to your friend
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":     "Merchant registered successfully",
		"merchant_id": merchant.ID, // Spits back the generated UUID string
		"store_name":  merchant.StoreName,
		"api_key":     merchant.APIKey,
	})
}