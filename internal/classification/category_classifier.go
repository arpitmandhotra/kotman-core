package classification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ProductCategoryCache is the GORM model for the product_category_cache table.
// Export it so cmd/migrate/main.go can AutoMigrate it.
type ProductCategoryCache struct {
	ProductTitleHash string    `gorm:"primaryKey;type:varchar(64)"` // SHA-256 hex
	CategoryL1       string    `gorm:"type:varchar(50)"`
	CategoryL2       string    `gorm:"type:varchar(50)"`
	ClassifiedAt     time.Time
}

func (ProductCategoryCache) TableName() string {
	return "product_category_cache"
}

// validTaxonomy is the fixed product taxonomy used for classification and validation.
var validTaxonomy = map[string][]string{
	"Apparel":     {"Ethnic Wear", "Western Wear", "Innerwear", "Activewear"},
	"Footwear":    {"Casual", "Formal", "Sports", "Sandals"},
	"Electronics": {"Mobile Accessories", "Smartphones", "Audio", "Wearables"},
	"Cosmetics":   {"Skincare", "Makeup", "Haircare", "Fragrance"},
	"Home":        {"Kitchen", "Decor", "Bedding", "Storage"},
	"FMCG":        {"Food", "Personal Care", "Household"},
}

const (
	geminiModel = "gemini-2.5-flash"
)

const systemPrompt = `You are a product classifier. Given a product title, respond with ONLY a JSON object {"category_l1": "...", "category_l2": "..."} from the fixed taxonomy. If the product doesn't fit any category, use category_l1="Other" and category_l2="Other". No other text.

Taxonomy:
Apparel > Ethnic Wear, Western Wear, Innerwear, Activewear
Footwear > Casual, Formal, Sports, Sandals
Electronics > Mobile Accessories, Smartphones, Audio, Wearables
Cosmetics > Skincare, Makeup, Haircare, Fragrance
Home > Kitchen, Decor, Bedding, Storage
FMCG > Food, Personal Care, Household`

// geminiRequest is the request body for the Gemini API.
type geminiRequest struct {
	Contents          []geminiContent    `json:"contents"`
	SystemInstruction *geminiInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiConfig      `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiConfig struct {
	ResponseMimeType string `json:"responseMimeType,omitempty"`
	MaxOutputTokens  int    `json:"maxOutputTokens,omitempty"`
}

// geminiResponse is the response body from the Gemini API.
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// classificationResult is the parsed JSON output from the LLM.
type classificationResult struct {
	CategoryL1 string `json:"category_l1"`
	CategoryL2 string `json:"category_l2"`
}

// ClassifyProduct classifies a product title into L1/L2 categories using
// a cache-first strategy backed by the Gemini API.
func ClassifyProduct(ctx context.Context, productTitle string, db *gorm.DB) (categoryL1, categoryL2 string, err error) {
	normalized := strings.ToLower(strings.TrimSpace(productTitle))
	hash := sha256Hash(normalized)

	// 1. Cache check FIRST
	var cached ProductCategoryCache
	result := db.WithContext(ctx).Raw(
		"SELECT category_l1, category_l2 FROM product_category_cache WHERE product_title_hash = ?",
		hash,
	).Scan(&cached)
	if result.Error == nil && result.RowsAffected > 0 {
		return cached.CategoryL1, cached.CategoryL2, nil
	}

	// 2. Gemini API call (with one retry on taxonomy mismatch)
	categoryL1, categoryL2, err = callGemini(ctx, productTitle)
	if err != nil {
		return "", "", fmt.Errorf("classification: gemini call failed: %w", err)
	}

	if !isValidTaxonomy(categoryL1, categoryL2) {
		// Retry ONCE
		categoryL1, categoryL2, err = callGemini(ctx, productTitle)
		if err != nil {
			return "", "", fmt.Errorf("classification: gemini retry failed: %w", err)
		}
		if !isValidTaxonomy(categoryL1, categoryL2) {
			return "", "", nil
		}
	}

	// 3. Cache the result via UPSERT
	now := time.Now()
	upsertSQL := `INSERT INTO product_category_cache (product_title_hash, category_l1, category_l2, classified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (product_title_hash) DO UPDATE
		SET category_l1 = EXCLUDED.category_l1, category_l2 = EXCLUDED.category_l2, classified_at = EXCLUDED.classified_at`
	if err := db.WithContext(ctx).Exec(upsertSQL, hash, categoryL1, categoryL2, now).Error; err != nil {
		return categoryL1, categoryL2, fmt.Errorf("classification: cache upsert failed: %w", err)
	}

	return categoryL1, categoryL2, nil
}

// callGemini calls the Gemini API and parses the classification result.
func callGemini(ctx context.Context, productTitle string) (string, string, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return "", "", fmt.Errorf("GEMINI_API_KEY is not set")
	}

	reqBody := geminiRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: fmt.Sprintf("Product title to classify: %s", productTitle)},
				},
			},
		},
		SystemInstruction: &geminiInstruction{
			Parts: []geminiPart{
				{Text: systemPrompt},
			},
		},
		GenerationConfig: &geminiConfig{
			ResponseMimeType: "application/json",
			MaxOutputTokens:  200,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	apiURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", geminiModel, apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}

	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return "", "", fmt.Errorf("empty response from gemini")
	}

	var cr classificationResult
	text := strings.TrimSpace(apiResp.Candidates[0].Content.Parts[0].Text)
	if err := json.Unmarshal([]byte(text), &cr); err != nil {
		return "", "", fmt.Errorf("parse classification JSON: %w (raw output: %q)", err, text)
	}

	return cr.CategoryL1, cr.CategoryL2, nil
}

// isValidTaxonomy checks whether the L1/L2 pair exists in the fixed taxonomy.
// "Other"/"Other" is always valid as the fallback category.
func isValidTaxonomy(l1, l2 string) bool {
	if l1 == "Other" && l2 == "Other" {
		return true
	}
	subcategories, ok := validTaxonomy[l1]
	if !ok {
		return false
	}
	for _, sc := range subcategories {
		if sc == l2 {
			return true
		}
	}
	return false
}

// sha256Hash returns the lowercase hex-encoded SHA-256 hash of s.
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
