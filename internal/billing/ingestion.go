package billing

import (
	"bytes"
	"context"
	"errors"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/classification"
	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type RedisClient interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// Shared singletons initialized at startup in cmd/main.go
var (
	DB    *gorm.DB
	Redis RedisClient
)

func currentBillingMonth() string {
	return time.Now().Format("2006-01")
}

// ParseAmountToPaise converts a raw float, int, or string amount representation to paise.
func ParseAmountToPaise(raw interface{}) (int, error) {
	if raw == nil {
		return 0, fmt.Errorf("amount is nil")
	}

	var val float64
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("amount cannot be NaN or Inf")
		}
		val = v
	case float32:
		f64 := float64(v)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			return 0, fmt.Errorf("amount cannot be NaN or Inf")
		}
		val = f64
	case int:
		val = float64(v)
	case int64:
		val = float64(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, fmt.Errorf("empty amount string")
		}

		// Reject scientific notations (e, E)
		if strings.ContainsAny(s, "eE") {
			return 0, fmt.Errorf("scientific notation is not allowed")
		}

		// Strip allowed currency indicators and comma grouping separators
		replacer := strings.NewReplacer(
			"₹", "",
			"$", "",
			",", "",
			"Rs.", "",
			"rs.", "",
			"RS.", "",
			"Rs", "",
			"rs", "",
			"RS", "",
			"INR", "",
			"inr", "",
			"Inr", "",
		)
		sCleaned := replacer.Replace(s)
		sCleaned = strings.TrimSpace(sCleaned)

		// Reject strings containing characters other than digits and at most one decimal point
		hasDecimal := false
		for _, char := range sCleaned {
			if char == '.' {
				if hasDecimal {
					return 0, fmt.Errorf("multiple decimal points in amount")
				}
				hasDecimal = true
			} else if char < '0' || char > '9' {
				return 0, fmt.Errorf("invalid character %q in amount", char)
			}
		}

		var err error
		val, err = strconv.ParseFloat(sCleaned, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse cleaned amount string %q: %w", v, err)
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, fmt.Errorf("parsed amount cannot be NaN or Inf")
		}
	default:
		return 0, fmt.Errorf("unsupported amount type: %T", raw)
	}

	if val < 0 {
		return 0, fmt.Errorf("amount cannot be negative: %f", val)
	}
	if val == 0 {
		return 0, fmt.Errorf("amount cannot be zero")
	}

	// Enforce the integer paise boundary protection strictly before converting
	// sanity cap: 10,000,000 paise (₹1,00,000), which corresponds to 100,000.00
	if val > 100000.00 {
		return 0, fmt.Errorf("amount exceeds maximum allowed limit: %f", val)
	}

	paise := int(math.Round(val * 100.0))

	if paise > 10000000 {
		return 0, fmt.Errorf("amount exceeds sanity cap of 10,000,000 paise: %d", paise)
	}

	return paise, nil
}

func getString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		val := m[key]
		if val == nil {
			continue
		}
		if s, ok := val.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", val)
	}
	return ""
}

// ProcessInboundOrder handles order ingestion, mode detection, fee computation, and database updates.
func ProcessInboundOrder(ctx context.Context, platform string, merchantID string, rawBody []byte) error {
	if DB == nil {
		return fmt.Errorf("database client not initialized in billing package")
	}

	// STEP 1: Parse the raw payload into an internal OrderPayload struct
	var rawPayload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	if err := dec.Decode(&rawPayload); err != nil {
		return fmt.Errorf("failed to unmarshal raw JSON body: %w", err)
	}

	payload := OrderPayload{
		Platform: strings.ToLower(platform),
		RawJSON:  string(rawBody),
	}

	noteAttrs := make(map[string]string)
	var tags []string
	var sourceName string

	switch payload.Platform {
	case "shopify":
		payload.PlatformOrderID = getString(rawPayload, "id")
		payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["total_price"])
		payload.PaymentMethod = DetectPaymentMethod(platform, rawPayload)

		// Phone: prefer billing_address.phone, fall back to customer.phone
		if billAddr, ok := rawPayload["billing_address"].(map[string]interface{}); ok {
			payload.PhoneRaw = getString(billAddr, "phone")
		}
		if payload.PhoneRaw == "" {
			if cust, ok := rawPayload["customer"].(map[string]interface{}); ok {
				payload.PhoneRaw = getString(cust, "phone")
			}
		}

		// Note attributes
		if attrs, ok := rawPayload["note_attributes"].([]interface{}); ok {
			for _, attr := range attrs {
				if attrMap, ok := attr.(map[string]interface{}); ok {
					name := getString(attrMap, "name")
					val := getString(attrMap, "value")
					if name != "" {
						noteAttrs[name] = val
					}
				}
			}
		}

		// Tags
		if tagsStr, ok := rawPayload["tags"].(string); ok && tagsStr != "" {
			parts := strings.Split(tagsStr, ",")
			for _, part := range parts {
				tags = append(tags, strings.TrimSpace(part))
			}
		}

		payload.SourceName = getString(rawPayload, "source_name")

	case "woocommerce":
		payload.PlatformOrderID = getString(rawPayload, "id")
		payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["total"])
		payload.PaymentMethod = DetectPaymentMethod(platform, rawPayload)

		if billingObj, ok := rawPayload["billing"].(map[string]interface{}); ok {
			payload.PhoneRaw = getString(billingObj, "phone")
		}

		// Meta data
		if metaList, ok := rawPayload["meta_data"].([]interface{}); ok {
			for _, meta := range metaList {
				if metaMap, ok := meta.(map[string]interface{}); ok {
					key := getString(metaMap, "key")
					val := metaMap["value"]
					if key != "" && val != nil {
						noteAttrs[key] = fmt.Sprintf("%v", val)
					}
				}
			}
		}

		if cv, ok := noteAttrs["_created_via"]; ok {
			sourceName = cv
		} else if gk, ok := noteAttrs["_gokwik_source"]; ok {
			sourceName = gk
		}
		payload.SourceName = sourceName

	case "magento":
		payload.PlatformOrderID = getString(rawPayload, "increment_id")
		payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["grand_total"])
		payload.PaymentMethod = DetectPaymentMethod(platform, rawPayload)

		if billAddr, ok := rawPayload["billing_address"].(map[string]interface{}); ok {
			payload.PhoneRaw = getString(billAddr, "telephone")
		}

		if extAttrs, ok := rawPayload["extension_attributes"].(map[string]interface{}); ok {
			for k, v := range extAttrs {
				noteAttrs[k] = fmt.Sprintf("%v", v)
			}
		}

		sourceName = getString(rawPayload, "remote_ip")
		if sourceName == "" {
			sourceName = getString(rawPayload, "x_forwarded_for")
		}
		payload.SourceName = sourceName

	default: // custom/generic
		payload.PlatformOrderID = getString(rawPayload, "order_id", "id")
		payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["order_value"])
		if payload.OrderValuePaise == 0 {
			payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["amount"])
		}
		if payload.OrderValuePaise == 0 {
			payload.OrderValuePaise, _ = ParseAmountToPaise(rawPayload["total"])
		}

		payload.PaymentMethod = DetectPaymentMethod(platform, rawPayload)
		payload.PhoneRaw = getString(rawPayload, "phone", "phone_number")
		payload.SourceName = getString(rawPayload, "source_name", "source")
	}

	payload.NoteAttributes = noteAttrs
	payload.Tags = tags

	// STEP 2: Idempotency check
	var existing domain.BillableEvent
	err := DB.WithContext(ctx).
		Where("merchant_id = ? AND platform = ? AND order_id = ?", merchantID, platform, payload.PlatformOrderID).
		First(&existing).Error
	if err == nil {
		slog.Info("duplicate order event received, skipping", "merchant_id", merchantID, "order_id", payload.PlatformOrderID, "platform", platform)
		return nil
	}

	// STEP 3: Detect checkout mode and payment method
	var merchantSettings domain.MerchantSettings
	if err := DB.WithContext(ctx).Where("merchant_id = ?", merchantID).First(&merchantSettings).Error; err != nil {
		merchantSettings = domain.MerchantSettings{
			MerchantID:         merchantID,
			CheckoutMode:       "native",
			ThirdPartyCheckout: "",
			BillingCycleDay:    1,
			AutoInvoiceEnabled: true,
		}
	}

	checkoutResult := DetectCheckoutMode(payload, merchantSettings)

	// STEP 4: Compute fee
	isBillable, feePaise := ComputeFee(checkoutResult.Mode, payload.PaymentMethod, payload.OrderValuePaise)

	// STEP 5: Hash phone number
	phoneHash := ""
	if payload.PhoneRaw != "" {
		phoneHash = crypto.HashPhone(payload.PhoneRaw)
	}

	// STEP 6: Create BillableEvent in a single INSERT
	event := domain.BillableEvent{
		MerchantID:      merchantID,
		OrderID:         payload.PlatformOrderID,
		Platform:        platform,
		CheckoutMode:    checkoutResult.Mode,
		ThirdPartyName:  checkoutResult.ThirdPartyName,
		PaymentMethod:   payload.PaymentMethod,
		OrderValuePaise: payload.OrderValuePaise,
		FeePaise:        feePaise,
		IsBillable:      isBillable,
		RawWebhookBody:  payload.RawJSON,
		PhoneHash:       phoneHash,
		RequiresReview:  checkoutResult.RequiresReview,
	}

	if err := DB.WithContext(ctx).Create(&event).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			slog.Warn("Duplicate webhook skipped (unique constraint violation 23505)", "merchant_id", merchantID, "order_id", payload.PlatformOrderID)
			return nil
		}
		return fmt.Errorf("failed to create BillableEvent: %w", err)
	}

	// STEP 7: If isBillable, increment the merchant's running monthly total atomically
	if isBillable {
		month := currentBillingMonth()
		var accumulator domain.MerchantBillingAccumulator
		err = DB.WithContext(ctx).
			Where("merchant_id = ? AND billing_month = ?", merchantID, month).
			FirstOrCreate(&accumulator, domain.MerchantBillingAccumulator{
				MerchantID:   merchantID,
				BillingMonth: month,
			}).Error
		if err != nil {
			return fmt.Errorf("failed to locate or create billing accumulator: %w", err)
		}

		result := DB.WithContext(ctx).Model(&domain.MerchantBillingAccumulator{}).
			Where("merchant_id = ? AND billing_month = ?", merchantID, month).
			Updates(map[string]interface{}{
				"total_events":    gorm.Expr("total_events + 1"),
				"total_fee_paise": gorm.Expr("total_fee_paise + ?", feePaise),
			})
		if result.Error != nil {
			return fmt.Errorf("failed to increment billing accumulator: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("billing accumulator update affected 0 rows (race condition check)")
		}

		// Perform prepaid wallet balance deduction using integer paise.
		// For Postgres, we rely on the check_positive_balance constraint. For SQLite, we query with threshold.
		deductQuery := DB.WithContext(ctx).Model(&domain.MerchantSettings{}).Where("merchant_id = ?", merchantID)
		if DB.Dialector.Name() != "postgres" {
			deductQuery = deductQuery.Where("wallet_balance_paise >= ?", feePaise)
		}

		deductRes := deductQuery.Update("wallet_balance_paise", gorm.Expr("wallet_balance_paise - ?", feePaise))
		if deductRes.Error != nil {
			var pgErr *pgconn.PgError
			if errors.As(deductRes.Error, &pgErr) && pgErr.Code == "23514" {
				slog.Error("ledger integrity check failed: insufficient merchant wallet balance (CHECK constraint check_positive_balance violated)", "merchant_id", merchantID, "fee", float64(feePaise)/100.0)
				// Gracefully skip deduction but return nil to complete webhook processing successfully
			} else {
				return fmt.Errorf("failed to deduct prepaid fee from wallet: %w", deductRes.Error)
			}
		} else if deductRes.RowsAffected == 0 {
			slog.Warn("insufficient wallet balance (skipped deduction during ingestion)", "merchant_id", merchantID, "fee", float64(feePaise)/100.0)
			// Continue webhook processing normally
		}
	}

	slog.Info("billable event recorded",
		"merchant_id", merchantID,
		"order_id", payload.PlatformOrderID,
		"platform", platform,
		"checkout_mode", checkoutResult.Mode,
		"third_party", checkoutResult.ThirdPartyName,
		"payment_method", payload.PaymentMethod,
		"order_value_paise", payload.OrderValuePaise,
		"fee_paise", feePaise,
		"is_billable", isBillable,
		"detection_path", checkoutResult.DetectionPath,
		"confidence", checkoutResult.Confidence,
		"requires_review", checkoutResult.RequiresReview,
	)

	// ═══════════════════════════════════════════════════════════════
	// SIGNALS SUBSYSTEM — Async classification & geo enrichment
	// Runs in a separate goroutine with its own 10s timeout.
	// Never blocks the webhook response. If classification fails,
	// log and move on — missing data just excludes from aggregation.
	// ═══════════════════════════════════════════════════════════════
	go func(eventID uint, rawJSON string) {
		classCtx, classCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer classCancel()

		// Parse line_items and shipping_address from the stored webhook body
		var webhookData struct {
			LineItems []struct {
				Title string      `json:"title"`
				Price interface{} `json:"price"`
			} `json:"line_items"`
			ShippingAddress struct {
				Province string `json:"province"`
			} `json:"shipping_address"`
		}
		if err := json.Unmarshal([]byte(rawJSON), &webhookData); err != nil {
			slog.Warn("signals: failed to parse webhook body for classification",
				"event_id", eventID, "error", err)
			return
		}

		// Pick the highest-value line item as the classification target
		bestTitle := ""
		bestPrice := 0.0
		for _, item := range webhookData.LineItems {
			price := 0.0
			if item.Price != nil {
				price, _ = parseLineItemPrice(item.Price)
			}
			if price > bestPrice || bestTitle == "" {
				bestPrice = price
				bestTitle = item.Title
			}
		}
		if bestTitle == "" {
			slog.Debug("signals: no line items found for classification", "event_id", eventID)
			return
		}

		// Classify product via LLM (cached — won't call API for repeated titles)
		catL1, catL2, classErr := classification.ClassifyProduct(classCtx, bestTitle, DB)
		if classErr != nil {
			slog.Warn("signals: product classification failed",
				"event_id", eventID, "title", bestTitle, "error", classErr)
			// Continue with geo extraction even if classification fails
		}

		// Extract geo from shipping_address.province
		geoState, geoTier := classification.LookupGeoTier(webhookData.ShippingAddress.Province)

		// Single UPDATE — no read-modify-write
		updateFields := map[string]interface{}{
			"category_l1": catL1,
			"category_l2": catL2,
			"geo_state":   geoState,
			"geo_tier":    geoTier,
		}
		result := DB.WithContext(classCtx).Model(&domain.BillableEvent{}).
			Where("id = ?", eventID).
			Updates(updateFields)
		if result.Error != nil {
			slog.Warn("signals: failed to update BillableEvent with classification",
				"event_id", eventID, "error", result.Error)
			return
		}

		slog.Info("signals: enriched order with classification + geo",
			"event_id", eventID,
			"category_l1", catL1,
			"category_l2", catL2,
			"geo_state", geoState,
			"geo_tier", geoTier,
		)
	}(event.ID, payload.RawJSON)

	return nil
}

// parseLineItemPrice extracts a float64 price from various JSON representations
func parseLineItemPrice(raw interface{}) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	case json.Number:
		return v.Float64()
	default:
		return 0, fmt.Errorf("unsupported price type: %T", raw)
	}
}

// ComputeFee returns whether an event is billable and what its fee is.
func ComputeFee(checkoutMode, paymentMethod string, orderValuePaise int) (bool, int) {
	switch checkoutMode {
	case "native":
		return true, domain.KotmanFee(orderValuePaise)
	case "third_party":
		if paymentMethod == "cod" {
			return true, domain.KotmanFee(orderValuePaise)
		}
		return false, 0
	default:
		return true, domain.KotmanFee(orderValuePaise)
	}
}

// ProcessOrderCreditBack handles RTO or cancelled orders by waiving the fee and crediting it back to the merchant's wallet balance.
func ProcessOrderCreditBack(ctx context.Context, platform, merchantID, orderID string) error {
	// Execute in a transaction to guarantee ledger consistency
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event domain.BillableEvent
		err := tx.Where("merchant_id = ? AND platform = ? AND order_id = ?", merchantID, platform, orderID).First(&event).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				slog.Info("no matching billable event found for cancellation/RTO credit back", "merchant_id", merchantID, "platform", platform, "order_id", orderID)
				return nil
			}
			return fmt.Errorf("failed to look up billable event: %w", err)
		}

		// If it's already waived/refunded or wasn't billable to begin with, no action needed
		if !event.IsBillable || event.FeePaise == 0 {
			slog.Info("event is not billable or already credited back, skipping", "merchant_id", merchantID, "order_id", orderID)
			return nil
		}

		// Keep a record of the original fee before waiving
		waivedFeePaise := event.FeePaise

		// Update event to waived state atomically, marking it as RTO
		updateResult := tx.Model(&domain.BillableEvent{}).
			Where("id = ? AND is_billable = ? AND fee_paise > ?", event.ID, true, 0).
			Updates(map[string]interface{}{
				"is_billable": false,
				"fee_paise":   0,
				"is_rto":      true,
			})
		if updateResult.Error != nil {
			return fmt.Errorf("failed to mark event as waived: %w", updateResult.Error)
		}
		if updateResult.RowsAffected == 0 {
			// Another goroutine already processed this cancellation — idempotent exit
			slog.Info("credit back already processed (concurrent request), skipping", 
				"merchant_id", merchantID, "order_id", orderID)
			return nil
		}

		// Decrement accumulator totals for the month the event was created in
		billingMonth := event.CreatedAt.Format("2006-01")
		var accumulator domain.MerchantBillingAccumulator
		err = tx.Where("merchant_id = ? AND billing_month = ?", merchantID, billingMonth).First(&accumulator).Error
		if err == nil {
			// Atomically decrement accumulator fields
			updateRes := tx.Model(&accumulator).Updates(map[string]interface{}{
				"total_events":    gorm.Expr("total_events - 1"),
				"total_fee_paise": gorm.Expr("total_fee_paise - ?", waivedFeePaise),
			})
			if updateRes.Error != nil {
				return fmt.Errorf("failed to decrement billing accumulator: %w", updateRes.Error)
			}
		}

		// Refund the equivalent paise amount to the merchant's wallet
		refundRes := tx.Model(&domain.MerchantSettings{}).
			Where("merchant_id = ?", merchantID).
			Update("wallet_balance_paise", gorm.Expr("wallet_balance_paise + ?", waivedFeePaise))
		if refundRes.Error != nil {
			return fmt.Errorf("failed to credit back wallet balance: %w", refundRes.Error)
		}

		slog.Info("successfully processed cancellation/RTO credit back",
			"merchant_id", merchantID,
			"order_id", orderID,
			"credited_amount_rupees", float64(waivedFeePaise)/100.0,
		)
		return nil
	})
}
