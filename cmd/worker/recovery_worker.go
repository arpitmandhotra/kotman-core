package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crm"
	"github.com/arpitmandhotra/api-integrator/internal/crypto"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/integrations/shopify"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type RecoveryWorker struct {
	redis *redis.Client
	pg    *gorm.DB
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("Starting Kaughtman Recovery Worker with CRM routing...")

	redisClient := database.NewRedisClient()
	postgresClient := database.NewPostgresClient()

	worker := &RecoveryWorker{
		redis: redisClient,
		pg:    postgresClient,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Verify keyspace notifications are enabled safely with v9 map mapping.
	// Upstash blocks CONFIG GET so we warn rather than fatal on that error.
	configVal, err := redisClient.ConfigGet(ctx, "notify-keyspace-events").Result()
	if err != nil {
		slog.Warn("could not verify Redis keyspace config (Upstash blocks CONFIG GET) — ensure 'Ex' is set in dashboard")
	} else {
		eventsConfig := configVal["notify-keyspace-events"]
		if !strings.Contains(eventsConfig, "E") {
			slog.Error("CRITICAL: Redis keyspace notifications not enabled — worker will receive no events")
			os.Exit(1)
		}
	}

	pubsub := redisClient.Subscribe(ctx, "__keyevent@0__:expired")
	defer pubsub.Close()

	slog.Info("Subscribed to Redis keyspace expiry events")

	go worker.listenAndProcess(ctx, pubsub)
	go worker.startTokenRefresher(ctx)
	go worker.runSubscriptionExpiryJob(ctx)   // NEW — runs every 6 hours
	go StartAIIngestionWorker(ctx, redisClient, postgresClient)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	slog.Info("Shutdown signal received — draining worker...")
	cancel()
	time.Sleep(1 * time.Second)
	slog.Info("Recovery worker terminated cleanly")
}

func (w *RecoveryWorker) listenAndProcess(ctx context.Context, pubsub *redis.PubSub) {
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				slog.Error("Redis PubSub channel closed unexpectedly")
				return
			}
			go w.handleExpiredKey(ctx, msg.Payload)
		}
	}
}

// handleExpiredKey parses the trigger key and routes to the correct processor.
// Key formats (hash in key, raw phone in separate data key):
//
//	cart:trigger:<merchant_id>:<phone_hash>
//	feedback:trigger:<merchant_id>:<phone_hash>:<order_id>
func (w *RecoveryWorker) handleExpiredKey(ctx context.Context, key string) {
	// Idempotency lock — prevents duplicate processing when Redis delivers
	// the same expiry event twice (cluster failovers, network retries,
	// or multiple worker replicas).
	lockKey := "lock:" + key
	ok, err := w.redis.SetNX(ctx, lockKey, "1", 5*time.Minute).Result()
	if err != nil || !ok {
		return // another worker/goroutine already processing this event
	}
	defer w.redis.Del(ctx, lockKey)

	parts := strings.Split(key, ":")
	if len(parts) < 4 {
		return // not our key — ignore silently
	}

	prefix := parts[0]
	action := parts[1]
	merchantID := parts[2]
	phoneHash := parts[3]

	switch {
	case prefix == "cart" && action == "trigger":
		w.processCartRecovery(ctx, merchantID, phoneHash)
	case prefix == "feedback" && action == "trigger" && len(parts) == 5:
		w.processPostPurchaseFeedback(ctx, merchantID, phoneHash, parts[4])
	}
}

func (w *RecoveryWorker) processCartRecovery(ctx context.Context, merchantID, phoneHash string) {
	slog.Info("processing cart recovery", "merchant_id", merchantID, "hash", safePreview(phoneHash))

	// 1. Load merchant CRM + billing settings
	settings, err := w.loadSettings(merchantID)
	if err != nil {
		slog.Error("failed to load merchant settings", "merchant_id", merchantID, "error", err)
		return
	}

	merchant, err := w.loadMerchant(merchantID)
	if err != nil {
		slog.Error("failed to load merchant for module check", "merchant_id", merchantID, "error", err)
		return
	}

	// 2. Retrieve raw phone from data key — set at cart abandonment time by JS snippet.
	// The data key expires slightly after the trigger key so it is still readable here.
	rawPhone, err := w.fetchAndDeleteDataKey(ctx,
		fmt.Sprintf("cart:data:%s:%s", merchantID, phoneHash))
	if err != nil {
		return // logged inside fetchAndDeleteDataKey
	}

	// 3. Load trust profile — may not exist for brand-new network buyers.
	// FIX: nil profile returned for ALL errors including real DB failures.
	// Caller checks profileErr == nil before trusting profile fields.
	profile, profileErr := w.loadProfile(phoneHash)

	// 4. Risk gate — skip high-risk buyers entirely, never attempt recovery.
	if profileErr == nil {
		if profile.TotalRTOs > 2 ||
			(profile.ComplaintCount > 0 && profile.ComplaintScore < -0.3) {
			slog.Warn("skipping cart recovery — high risk buyer",
				"hash", safePreview(phoneHash),
				"rto_count", profile.TotalRTOs,
			)
			return
		}
	}

	// 5. Choose template based on buyer history
	template := "STANDARD_CART_RECOVERY"
	isVIP := false
	riskScore := 85.0 // safe default for unknown buyers

	if profileErr == nil {
		riskScore = w.computeRiskScore(profile)
		if profile.TotalRTOs == 0 && profile.ComplaintCount == 0 {
			template = "VIP_RECOVERY_PROMPTED"
			isVIP = true
		}
	}

	// Determine segment tag from profile data
	segmentTag := "high_intent" // default for unknown buyers
	if profileErr == nil {
		switch {
		case profile.IsBlacklisted || profile.TotalRTOs > 3:
			segmentTag = "rto_risk"
		case profile.TotalRTOs == 0 && profile.ComplaintCount == 0 && profile.TotalOrders > 5:
			segmentTag = "vip_buyer"
		case profile.TotalRTOs == 0 && profile.TotalOrders > 0:
			segmentTag = "prepaid_candidate"
		default:
			segmentTag = "high_intent"
		}
	}

	event := crm.KaughtmanRiskEvent{
		PhoneHash:  phoneHash,
		MerchantID: merchantID,
		Template:   template,
		RiskScore:  riskScore,
		RTOCount:   w.safeRTOCount(profile, profileErr),
		IsVIP:      isVIP,
		EventTime:  time.Now(),
		SegmentTag: segmentTag,
	}

	// 6. Route to CRM or fall back through direct WhatsApp tiers
	w.executeRouting(ctx, merchant, settings, rawPhone, phoneHash, event, "your cart")
}

func (w *RecoveryWorker) processPostPurchaseFeedback(ctx context.Context, merchantID, phoneHash, orderID string) {
	slog.Info("processing post-purchase feedback", "order_id", orderID, "hash", safePreview(phoneHash))

	settings, err := w.loadSettings(merchantID)
	if err != nil {
		slog.Error("failed to load merchant settings", "merchant_id", merchantID, "error", err)
		return
	}

	merchant, err := w.loadMerchant(merchantID)
	if err != nil {
		slog.Error("failed to load merchant for module check", "merchant_id", merchantID, "error", err)
		return
	}

	rawPhone, err := w.fetchAndDeleteDataKey(ctx,
		fmt.Sprintf("feedback:data:%s:%s:%s", merchantID, phoneHash, orderID))
	if err != nil {
		return
	}

	// FIX: nil profile returned for ALL errors — no partial zero-value structs
	profile, profileErr := w.loadProfile(phoneHash)

	template := "STANDARD_FEEDBACK_REQUEST"
	discount := 0
	isVIP := false
	riskScore := 85.0

	if profileErr == nil {
		riskScore = w.computeRiskScore(profile)
		if profile.TotalRTOs == 0 && profile.ComplaintCount == 0 {
			template = "INCENTIVIZED_VIP_FEEDBACK_COUPON"
			discount = 15
			isVIP = true
		}
	}

	// Determine segment tag from profile data
	segmentTag := "high_intent" // default for unknown buyers
	if profileErr == nil {
		switch {
		case profile.IsBlacklisted || profile.TotalRTOs > 3:
			segmentTag = "rto_risk"
		case profile.TotalRTOs == 0 && profile.ComplaintCount == 0 && profile.TotalOrders > 5:
			segmentTag = "vip_buyer"
		case profile.TotalRTOs == 0 && profile.TotalOrders > 0:
			segmentTag = "prepaid_candidate"
		default:
			segmentTag = "high_intent"
		}
	}

	event := crm.KaughtmanRiskEvent{
		PhoneHash:     phoneHash,
		MerchantID:    merchantID,
		Template:      template,
		DiscountValue: discount,
		RiskScore:     riskScore,
		RTOCount:      w.safeRTOCount(profile, profileErr),
		IsVIP:         isVIP,
		EventTime:     time.Now(),
		SegmentTag:    segmentTag,
	}

	// Load the order value from the database if available
	orderValueStr := "your order"
	var billableEvent domain.BillableEvent
	if err := w.pg.WithContext(ctx).Where("order_id = ? AND merchant_id = ?", orderID, merchantID).First(&billableEvent).Error; err == nil {
		orderValueStr = fmt.Sprintf("Rs. %.2f", float64(billableEvent.OrderValuePaise)/100.0)
	}

	w.executeRouting(ctx, merchant, settings, rawPhone, phoneHash, event, orderValueStr)
}

// executeRouting implements the three-tier routing hierarchy:
//
//	Tier 1 — CRM connector (Klaviyo / HubSpot / MoEngage / WebEngage)
//	Tier 2 — Merchant's own Twilio/Interakt key
//	Tier 3 — Kaughtman managed wallet (Twilio master key)
//
// Each tier falls through to the next on failure.
func (w *RecoveryWorker) executeRouting(
	ctx context.Context,
	merchant *domain.Merchant,
	settings *domain.MerchantSettings,
	rawPhone, phoneHash string,
	event crm.KaughtmanRiskEvent,
	orderValue string,
) {
	// TIER 1: CRM connector
	// CORRECT: CRM push requires MODULE 3 (HasCRMUpsellEngine)
	if merchant.HasCRMUpsellEngine && settings.CRMProvider != "" && settings.CRMAPIKey != "" {
		connector, err := crm.NewConnector(
			settings.CRMProvider,
			settings.CRMAPIKey,
			settings.CRMAccountID,
		)
		if err != nil {
			slog.Error("CRM connector init failed",
				"provider", settings.CRMProvider,
				"merchant_id", merchant.ID,
				"error", err,
			)
			// Fall through to Tier 2 — do NOT return
		} else {
			if err := connector.SyncRiskEvent(ctx, event); err != nil {
				slog.Error("CRM sync failed — falling through to direct WhatsApp",
					"crm", connector.Name(),
					"merchant_id", merchant.ID,
					"error", err,
				)
				// Fall through to Tier 2 — do NOT return
			} else {
				slog.Info("CRM sync successful — routing complete",
					"crm", connector.Name(),
					"merchant_id", merchant.ID,
				)
				return // CRM handled it — stop here
			}
		}
	} else if settings.CRMProvider != "" && !merchant.HasCRMUpsellEngine {
		// Merchant has CRM configured but hasn't purchased MODULE 3.
		// Log this as a debug event — useful for sales follow-up on upgrade.
		slog.Debug("CRM push skipped — merchant has not purchased CRM Upsell Engine module",
			"merchant_id", merchant.ID,
			"configured_provider", settings.CRMProvider,
		)
		// Fall through to Tier 2
	}

	// TIER 2: Merchant's own communications key
	if settings.HasOwnCommunicationsKey && settings.ProviderAPIKey != "" {
		slog.Info("routing via merchant own key",
			"provider", settings.ProviderName,
			"merchant_id", settings.MerchantID,
		)
		err := w.sendWhatsApp(ctx, rawPhone, phoneHash, event.Template, event.DiscountValue,
			settings.ProviderAPIKey, settings.ProviderName, merchant.StoreName, orderValue)
		if err == nil {
			return
		}
		slog.Error("Tier 2 WhatsApp send failed — falling through to Tier 3", "error", err)
	}

	// TIER 3: Kaughtman managed postpaid messaging
	const messageCostPaise = 100
	slog.Info("routing via Kaughtman managed postpaid messaging", "merchant_id", settings.MerchantID)

	masterKey := os.Getenv("KAUGHTMAN_MASTER_TWILIO_KEY")
	if masterKey == "" {
		slog.Error("KAUGHTMAN_MASTER_TWILIO_KEY not set — cannot send managed message")
		return
	}

	err := w.sendWhatsApp(ctx, rawPhone, phoneHash, event.Template, event.DiscountValue,
		masterKey, "twilio", merchant.StoreName, orderValue)
	if err == nil {
		// Increment the billing accumulator for the current month postpaid
		month := time.Now().Format("2006-01")
		var accumulator domain.MerchantBillingAccumulator
		_ = w.pg.Where("merchant_id = ? AND billing_month = ?", settings.MerchantID, month).
			FirstOrCreate(&accumulator, domain.MerchantBillingAccumulator{
				MerchantID:   settings.MerchantID,
				BillingMonth: month,
			}).Error

		w.pg.Model(&domain.MerchantBillingAccumulator{}).
			Where("merchant_id = ? AND billing_month = ?", settings.MerchantID, month).
			Updates(map[string]interface{}{
				"total_fee_paise": gorm.Expr("total_fee_paise + ?", messageCostPaise),
			})
		return
	}
	slog.Error("Tier 3 WhatsApp send failed", "error", err)

	slog.Warn("all routing tiers exhausted — message dropped",
		"merchant_id", settings.MerchantID,
		"hash", safePreview(phoneHash),
	)
}

// ==========================================
// HELPERS
// ==========================================

func (w *RecoveryWorker) loadSettings(merchantID string) (*domain.MerchantSettings, error) {
	var settings domain.MerchantSettings
	err := w.pg.Where("merchant_id = ?", merchantID).First(&settings).Error
	return &settings, err
}

// loadMerchant fetches the Merchant row for module entitlement checks.
// Returns (nil, err) for all error cases — callers must check err before accessing fields.
func (w *RecoveryWorker) loadMerchant(merchantID string) (*domain.Merchant, error) {
	var merchant domain.Merchant
	err := w.pg.
		Select("id", "is_active", "has_rto_engine", "has_crm_upsell_engine", "has_cross_network_intel").
		Where("id = ?", merchantID).
		First(&merchant).Error
	if err != nil {
		return nil, err
	}
	return &merchant, nil
}

// loadProfile returns (nil, err) for ALL error cases — both ErrRecordNotFound
// and real DB failures. This prevents a zero-value TrustProfile from being
// mistaken for a clean buyer when a database timeout occurs.
func (w *RecoveryWorker) loadProfile(phoneHash string) (*domain.TrustProfile, error) {
	var profile domain.TrustProfile
	err := w.pg.Where("phone_hash = ?", phoneHash).First(&profile).Error
	if err != nil {
		// Intentionally return nil for both "not found" and real errors.
		// Callers use profileErr == nil to gate all profile field access.
		return nil, err
	}
	return &profile, nil
}

func (w *RecoveryWorker) fetchAndDeleteDataKey(ctx context.Context, dataKey string) (string, error) {
	// Atomic get-and-delete — prevents two goroutines from both reading
	// the same raw phone before either deletes it.
	rawPhone, err := w.redis.GetDel(ctx, dataKey).Result()
	if err == redis.Nil {
		slog.Warn("data key missing — trigger fired but data already expired or consumed",
			"key", dataKey)
		return "", err
	}
	if err != nil {
		slog.Error("redis error fetching data key", "key", dataKey, "error", err)
		return "", err
	}
	return rawPhone, nil
}

func (w *RecoveryWorker) computeRiskScore(profile *domain.TrustProfile) float64 {
	if profile == nil {
		return 85.0 // unknown buyer — optimistic default
	}
	score := 85.0
	score += profile.RiskAdjustment // negative deltas applied by feedback weights
	if profile.TotalOrders > 0 {
		rtoRate := float64(profile.TotalRTOs) / float64(profile.TotalOrders)
		score -= rtoRate * 40 // high RTO rate pulls score down hard
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

func (w *RecoveryWorker) safeRTOCount(profile *domain.TrustProfile, err error) int {
	if err != nil || profile == nil {
		return 0
	}
	return profile.TotalRTOs
}

var (
	interaktAPIURL  = "https://api.interakt.ai/v1/public/message/"
	whatsappTimeout = 10 * time.Second
)

// sendWhatsApp dispatches a WhatsApp message via the specified provider.
// ctx is passed as first parameter for http.NewRequestWithContext.
func (w *RecoveryWorker) sendWhatsApp(
	ctx context.Context,
	rawPhone, phoneHash, template string,
	discount int,
	apiKey, providerName string,
	merchantName, orderValue string,
) error {
	// Mask phone — never log raw PII
	masked := "91**********"
	if len(rawPhone) > 8 {
		masked = rawPhone[:4] + "****" + rawPhone[len(rawPhone)-2:]
	}

	provider := os.Getenv("WHATSAPP_PROVIDER")
	if provider == "" {
		provider = providerName
	}

	slog.Info("whatsapp dispatch initiated",
		"recipient_masked", masked,
		"template", template,
		"incentive_pct", discount,
		"hash", safePreview(phoneHash),
		"provider", provider,
		"has_api_key", apiKey != "",
	)

	switch provider {
	case "twilio":
		slog.Debug("twilio dispatch stub — Twilio send not implemented yet")
		return nil

	case "interakt":
		interaktKey := os.Getenv("INTERAKT_API_KEY")
		if interaktKey == "" {
			interaktKey = apiKey
		}
		if interaktKey == "" {
			return fmt.Errorf("interakt: API key is not configured")
		}

		templateName := os.Getenv("WHATSAPP_TEMPLATE_NAME")
		if templateName == "" {
			templateName = template
		}

		// Strip country code from phone number if present
		phoneWithoutCountry := rawPhone
		if strings.HasPrefix(phoneWithoutCountry, "+91") {
			phoneWithoutCountry = strings.TrimPrefix(phoneWithoutCountry, "+91")
		} else if strings.HasPrefix(phoneWithoutCountry, "91") && len(phoneWithoutCountry) > 10 {
			phoneWithoutCountry = strings.TrimPrefix(phoneWithoutCountry, "91")
		}

		// Build request body
		type interaktTemplate struct {
			Name         string   `json:"name"`
			LanguageCode string   `json:"languageCode"`
			BodyValues   []string `json:"bodyValues"`
		}
		type interaktPayload struct {
			CountryCode  string           `json:"countryCode"`
			PhoneNumber  string           `json:"phoneNumber"`
			CallbackData string           `json:"callbackData"`
			Type         string           `json:"type"`
			Template     interaktTemplate `json:"template"`
		}

		payload := interaktPayload{
			CountryCode:  "+91",
			PhoneNumber:  phoneWithoutCountry,
			CallbackData: "kaughtman_rto_check",
			Type:         "Template",
			Template: interaktTemplate{
				Name:         templateName,
				LanguageCode: "en",
				BodyValues:   []string{merchantName, orderValue},
			},
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("interakt: marshal payload failed: %w", err)
		}

		// Set 10-second timeout context
		sendCtx, cancel := context.WithTimeout(ctx, whatsappTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, interaktAPIURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return fmt.Errorf("interakt: create request failed: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Basic "+interaktKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if sendCtx.Err() == context.DeadlineExceeded {
				slog.Error("whatsapp send failed: timeout sending to interakt", "error", err)
			} else {
				slog.Error("whatsapp send failed sending to interakt", "error", err)
			}
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			slog.Error("whatsapp send failed with client error (4xx), will not retry", "status", resp.StatusCode)
			return nil // do not retry
		}

		if resp.StatusCode >= 500 {
			slog.Error("whatsapp send failed with server error (5xx)", "status", resp.StatusCode)
			return fmt.Errorf("interakt provider returned 5xx status: %d", resp.StatusCode)
		}

		slog.Info("whatsapp message sent successfully via interakt", "recipient_masked", masked)
		return nil

	default:
		slog.Warn("unknown WhatsApp provider — message not sent", "provider", provider)
		return nil
	}
}

// safePreview returns the first 8 characters of a hash for logging,
// guarding against panics on short or empty strings.
func safePreview(hash string) string {
	if len(hash) < 8 {
		return hash
	}
	return hash[:8]
}

func (w *RecoveryWorker) startTokenRefresher(ctx context.Context) {
	slog.Info("Starting background Shopify token refresher...")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Run immediately on startup
	w.refreshShopifyTokens(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.refreshShopifyTokens(ctx)
		}
	}
}

func (w *RecoveryWorker) refreshShopifyTokens(ctx context.Context) {
	slog.Info("Running background Shopify token refresh checks...")
	var creds []domain.PlatformCredential
	// Find active Shopify credentials expiring within the next 10 minutes (or already expired)
	threshold := time.Now().Add(10 * time.Minute)
	err := w.pg.Where("platform = ? AND is_active = ? AND token_expires_at < ?", "shopify", true, threshold).Find(&creds).Error
	if err != nil {
		slog.Error("failed to query expiring shopify credentials", "error", err)
		return
	}

	if len(creds) == 0 {
		slog.Info("no expiring shopify credentials found")
		return
	}

	for _, cred := range creds {
		slog.Info("attempting to refresh shopify token", "shop", cred.ShopDomain, "merchant_id", cred.MerchantID)

		refreshToken, err := crypto.DecryptToken(cred.RefreshTokenEncrypted)
		if err != nil {
			slog.Error("failed to decrypt shopify refresh token", "shop", cred.ShopDomain, "merchant_id", cred.MerchantID, "error", err)
			continue
		}

		resp, err := shopify.RefreshAccessToken(ctx, cred.ShopDomain, refreshToken)
		if err != nil {
			slog.Error("shopify refresh API call failed", "shop", cred.ShopDomain, "merchant_id", cred.MerchantID, "error", err)
			continue
		}

		encAccess, err := crypto.EncryptToken(resp.AccessToken)
		if err != nil {
			slog.Error("failed to encrypt new shopify access token", "shop", cred.ShopDomain, "error", err)
			continue
		}

		var encRefresh string
		if resp.RefreshToken != "" {
			encRefresh, err = crypto.EncryptToken(resp.RefreshToken)
			if err != nil {
				slog.Error("failed to encrypt new shopify refresh token", "shop", cred.ShopDomain, "error", err)
				continue
			}
		} else {
			encRefresh = cred.RefreshTokenEncrypted
		}

		expiresAt := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
		now := time.Now()

		cred.AccessTokenEncrypted = encAccess
		cred.RefreshTokenEncrypted = encRefresh
		cred.TokenExpiresAt = &expiresAt
		cred.LastRefreshedAt = &now

		if err := w.pg.Save(&cred).Error; err != nil {
			slog.Error("failed to save refreshed shopify credentials", "shop", cred.ShopDomain, "merchant_id", cred.MerchantID, "error", err)
		} else {
			slog.Info("successfully refreshed shopify credentials", "shop", cred.ShopDomain, "merchant_id", cred.MerchantID)
		}
	}
}

// runSubscriptionExpiryJob checks for expired flat-fee module subscriptions
// and deactivates them. Runs on a ticker — should be called in a goroutine.
// It does NOT auto-renew — renewal requires a new Razorpay payment initiated
// by the merchant. On expiry, the module bool is set to false and the merchant
// receives a downgrade (analytics remain, paid features are gated off).
func (w *RecoveryWorker) runSubscriptionExpiryJob(ctx context.Context) {
	// Run immediately on startup, then every 6 hours
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	w.processExpiredSubscriptions(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("subscription expiry job shutting down")
			return
		case <-ticker.C:
			w.processExpiredSubscriptions(ctx)
		}
	}
}

func (w *RecoveryWorker) processExpiredSubscriptions(ctx context.Context) {
	slog.Info("running subscription expiry check")

	// Log subscriptions in grace period (ended but within 72 hours) — useful for dunning alerts
	var inGrace []domain.MerchantSubscription
	w.pg.WithContext(ctx).
		Where("status = ? AND current_period_end < ? AND current_period_end > ?",
			"active", time.Now(), time.Now().Add(-72*time.Hour)).
		Find(&inGrace)

	for _, sub := range inGrace {
		slog.Warn("subscription in grace period — renewal overdue",
			"merchant_id",  sub.MerchantID,
			"module",        sub.Module,
			"expired_at",    sub.CurrentPeriodEnd,
			"hard_expires",  sub.CurrentPeriodEnd.Add(72*time.Hour),
		)
		// TODO: trigger dunning email here when email infrastructure is in place
	}

	// 3-day grace period: only expire subscriptions that ended more than 72 hours ago
	graceDeadline := time.Now().Add(-72 * time.Hour)

	var expired []domain.MerchantSubscription
	if err := w.pg.WithContext(ctx).
		Where("status = ? AND current_period_end < ?", "active", graceDeadline).
		Find(&expired).Error; err != nil {
		slog.Error("failed to query expired subscriptions", "error", err)
		return
	}

	if len(expired) == 0 {
		slog.Info("no expired subscriptions found")
		return
	}

	slog.Info("found expired subscriptions", "count", len(expired))

	for _, sub := range expired {
		if err := w.expireSubscription(ctx, sub); err != nil {
			slog.Error("failed to expire subscription",
				"subscription_id", sub.ID,
				"merchant_id",     sub.MerchantID,
				"module",          sub.Module,
				"error",           err,
			)
			// Continue — don't let one failure block other expirations
		}
	}
}

func (w *RecoveryWorker) expireSubscription(ctx context.Context, sub domain.MerchantSubscription) error {
	return w.pg.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// IMPORTANT: Do NOT touch is_active or has_rto_engine here.
		// The RTO engine is funded by wallet balance, not a time-boxed subscription.
		// It only deactivates via the Postgres check_positive_balance constraint.

		// Idempotent exit: check status = 'active'
		result := tx.Model(&domain.MerchantSubscription{}).
			Where("id = ? AND status = ?", sub.ID, "active").
			Updates(map[string]interface{}{
				"status":       "inactive",
				"cancelled_at": time.Now(),
			})
		if result.Error != nil {
			return fmt.Errorf("failed to mark subscription inactive: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			// Another worker already expired this subscription — idempotent exit
			slog.Info("subscription already expired by concurrent worker", "subscription_id", sub.ID)
			return nil
		}

		// Determine which Merchant field to flip based on module name
		merchantUpdates := map[string]interface{}{}
		switch sub.Module {
		case domain.ModuleCrossNetwork:
			// Only revoke HasCrossNetworkIntel if the merchant does NOT have HasRTOEngine.
			// If they have the RTO engine, cross-network is still bundled and free.
			var merchant domain.Merchant
			if err := tx.Select("has_rto_engine").Where("id = ?", sub.MerchantID).First(&merchant).Error; err != nil {
				return fmt.Errorf("failed to load merchant for expiry check: %w", err)
			}
			if !merchant.HasRTOEngine {
				merchantUpdates["has_cross_network_intel"] = false
				merchantUpdates["cross_network_renews_at"] = nil
			} else {
				// RTO engine still active — cross-network stays on, just remove the sub record
				slog.Info("cross_network sub expired but RTO engine still active — keeping intel access",
					"merchant_id", sub.MerchantID,
				)
				return nil // Nothing to do on Merchant row
			}

		case domain.ModuleCRMUpsell:
			merchantUpdates["has_crm_upsell_engine"] = false
			merchantUpdates["crm_upsell_renews_at"]   = nil

		default:
			return fmt.Errorf("unknown module during expiry: %s", sub.Module)
		}

		if len(merchantUpdates) > 0 {
			if err := tx.Model(&domain.Merchant{}).
				Where("id = ?", sub.MerchantID).
				Updates(merchantUpdates).Error; err != nil {
				return fmt.Errorf("failed to update merchant flags on expiry: %w", err)
			}
		}

		slog.Info("subscription expired and module deactivated",
			"merchant_id", sub.MerchantID,
			"module",       sub.Module,
			"expired_at",   sub.CurrentPeriodEnd,
		)
		return nil
	})
}