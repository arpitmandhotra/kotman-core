package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/crm"
	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
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
	slog.Info("Starting Kotman Recovery Worker with CRM routing...")

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
	slog.Info("processing cart recovery", "merchant_id", merchantID, "hash", phoneHash[:8])

	// 1. Load merchant CRM + billing settings
	settings, err := w.loadSettings(merchantID)
	if err != nil {
		slog.Error("failed to load merchant settings", "merchant_id", merchantID, "error", err)
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
				"hash", phoneHash[:8],
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

	event := crm.KotmanRiskEvent{
		PhoneHash:  phoneHash,
		MerchantID: merchantID,
		Template:   template,
		RiskScore:  riskScore,
		RTOCount:   w.safeRTOCount(profile, profileErr),
		IsVIP:      isVIP,
		EventTime:  time.Now(),
	}

	// 6. Route to CRM or fall back through direct WhatsApp tiers
	w.executeRouting(ctx, settings, rawPhone, phoneHash, event)
}

func (w *RecoveryWorker) processPostPurchaseFeedback(ctx context.Context, merchantID, phoneHash, orderID string) {
	slog.Info("processing post-purchase feedback", "order_id", orderID, "hash", phoneHash[:8])

	settings, err := w.loadSettings(merchantID)
	if err != nil {
		slog.Error("failed to load merchant settings", "merchant_id", merchantID, "error", err)
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

	event := crm.KotmanRiskEvent{
		PhoneHash:     phoneHash,
		MerchantID:    merchantID,
		Template:      template,
		DiscountValue: discount,
		RiskScore:     riskScore,
		RTOCount:      w.safeRTOCount(profile, profileErr),
		IsVIP:         isVIP,
		EventTime:     time.Now(),
	}

	w.executeRouting(ctx, settings, rawPhone, phoneHash, event)
}

// executeRouting implements the three-tier routing hierarchy:
//
//	Tier 1 — CRM connector (Klaviyo / HubSpot / MoEngage / WebEngage)
//	Tier 2 — Merchant's own Twilio/Interakt key
//	Tier 3 — Kotman managed wallet (Twilio master key)
//
// Each tier falls through to the next on failure.
func (w *RecoveryWorker) executeRouting(
	ctx context.Context,
	settings *domain.MerchantSettings,
	rawPhone, phoneHash string,
	event crm.KotmanRiskEvent,
) {
	// TIER 1: CRM connector
	if settings.CRMProvider != "" && settings.CRMAPIKey != "" {
		connector, err := crm.NewConnector(
			settings.CRMProvider,
			settings.CRMAPIKey,
			settings.CRMAccountID,
		)
		if err != nil {
			slog.Error("CRM connector init failed",
				"provider", settings.CRMProvider,
				"error", err,
			)
			// Fall through to Tier 2
		} else {
			if err := connector.SyncRiskEvent(ctx, event); err != nil {
				slog.Error("CRM sync failed — falling through to direct WhatsApp",
					"crm", connector.Name(),
					"error", err,
				)
				// Fall through to Tier 2
			} else {
				return // CRM handled it — done
			}
		}
	}

	// TIER 2: Merchant's own communications key
	if settings.HasOwnCommunicationsKey && settings.ProviderAPIKey != "" {
		slog.Info("routing via merchant own key",
			"provider", settings.ProviderName,
			"merchant_id", settings.MerchantID,
		)
		w.sendWhatsApp(ctx, rawPhone, phoneHash, event.Template, event.DiscountValue,
			settings.ProviderAPIKey, settings.ProviderName)
		return
	}

	// TIER 3: Kotman managed wallet
	const messageCost = 1.00
	if settings.WalletBalance >= messageCost {
		slog.Info("routing via Kotman managed wallet", "merchant_id", settings.MerchantID)

		masterKey := os.Getenv("KOTMAN_MASTER_TWILIO_KEY")
		if masterKey == "" {
			slog.Error("KOTMAN_MASTER_TWILIO_KEY not set — cannot send managed message")
			return
		}

		// Atomic wallet deduction — only deducts if balance is still sufficient.
		// RowsAffected == 0 means another goroutine already spent the balance.
		result := w.pg.Model(settings).
			Where("wallet_balance >= ?", messageCost).
			Update("wallet_balance", gorm.Expr("wallet_balance - ?", messageCost))

		if result.Error != nil || result.RowsAffected == 0 {
			slog.Warn("wallet deduction failed or insufficient balance",
				"merchant_id", settings.MerchantID,
			)
			return
		}

		w.sendWhatsApp(ctx, rawPhone, phoneHash, event.Template, event.DiscountValue,
			masterKey, "twilio")
		return
	}

	slog.Warn("all routing tiers exhausted — message dropped",
		"merchant_id", settings.MerchantID,
		"hash", phoneHash[:8],
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
	rawPhone, err := w.redis.Get(ctx, dataKey).Result()
	if err == redis.Nil {
		slog.Warn("data key missing — trigger fired but data already expired",
			"key", dataKey)
		return "", err
	}
	if err != nil {
		slog.Error("redis error fetching data key", "key", dataKey, "error", err)
		return "", err
	}
	// Delete immediately — PII must not linger in Redis after use
	w.redis.Del(ctx, dataKey)
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

// sendWhatsApp dispatches a WhatsApp message via the specified provider.
// FIX: ctx added as first parameter so the real HTTP call can respect
// cancellation and timeouts without a refactor of the call sites.
//
// providerName: "twilio" | "interakt"
// Production: replace the slog stub with an HTTP POST to the provider's API.
func (w *RecoveryWorker) sendWhatsApp(
	ctx context.Context,
	rawPhone, phoneHash, template string,
	discount int,
	apiKey, providerName string,
) {
	// Mask phone — never log raw PII
	masked := "91**********"
	if len(rawPhone) > 8 {
		masked = rawPhone[:4] + "****" + rawPhone[len(rawPhone)-2:]
	}

	slog.Info("whatsapp dispatched",
		"recipient_masked", masked,
		"template", template,
		"incentive_pct", discount,
		"hash", phoneHash[:8],
		"provider", providerName,
	)

	// Production dispatch — switch on provider and POST to their API.
	// ctx is available here for http.NewRequestWithContext.
	switch providerName {
	case "twilio":
		// POST https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json
		// Body: To=whatsapp:+{rawPhone}&From=whatsapp:+{twilioNumber}&Body={templateText}
		// Auth: Basic base64(AccountSid:AuthToken) where apiKey = "AccountSid:AuthToken"
		_ = ctx // placeholder until implementation
		slog.Debug("twilio dispatch stub — implement HTTP POST here")

	case "interakt":
		// POST https://api.interakt.ai/v1/public/message/
		// Headers: Authorization: Basic {base64(apiKey)}
		// Body: {"countryCode":"+91","phoneNumber":"{rawPhone}","type":"Template",
		//         "template":{"name":"{template}","languageCode":"en"}}
		_ = ctx // placeholder until implementation
		slog.Debug("interakt dispatch stub — implement HTTP POST here")

	default:
		slog.Warn("unknown WhatsApp provider — message not sent", "provider", providerName)
	}
}