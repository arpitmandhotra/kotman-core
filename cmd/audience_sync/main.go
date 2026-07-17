package main

import (
    "context"
    "fmt"
    "log"
    "log/slog"
    "os"
    "time"

    "github.com/arpitmandhotra/api-integrator/internal/database"
    "github.com/arpitmandhotra/api-integrator/internal/domain"
    "github.com/arpitmandhotra/api-integrator/internal/integrations/meta"
    "github.com/joho/godotenv"
    "gorm.io/gorm"
)

func main() {
    if err := godotenv.Load(); err != nil {
        log.Printf("Warning: .env file not found: %v", err)
    }

    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    slog.SetDefault(logger)
    slog.Info("starting scheduled weekly custom audience sync job")

    pg := database.NewPostgresClient()

    if err := RunAudienceSync(pg); err != nil {
        slog.Error("audience sync failed with error", "error", err)
        os.Exit(1)
    }

    slog.Info("audience sync worker completed successfully")
}

func RunAudienceSync(pg *gorm.DB) error {
    ctx := context.Background()

    // 1. PRE-FLIGHT DENSITY CHECK
    var count int64
    err := pg.Model(&domain.MerchantSettings{}).
        Joins("JOIN merchants ON merchants.id::text = merchant_settings.merchant_id").
        Where("merchants.tier = ? AND merchant_settings.meta_capi_enabled = ? AND merchant_settings.meta_ad_account_id != ''", domain.TierGrowthAds, true).
        Count(&count).Error
    if err != nil {
        return fmt.Errorf("failed to count active Meta configured merchants: %w", err)
    }

    if count == 0 {
        slog.Info("audience_sync: no merchants with Meta configured on growth_ads tier, skipping")
        return nil
    }

    // 2. Fetch all active merchants with Meta CAPI enabled and ad account credentials on growth_ads tier
    var settingsList []domain.MerchantSettings
    err = pg.Joins("JOIN merchants ON merchants.id::text = merchant_settings.merchant_id").
        Where("merchants.tier = ? AND merchant_settings.meta_capi_enabled = ? AND merchant_settings.meta_ad_account_id != '' AND merchant_settings.meta_access_token != ''", domain.TierGrowthAds, true).
        Find(&settingsList).Error
    if err != nil {
        return fmt.Errorf("failed to fetch Meta settings: %w", err)
    }

    totalMerchantsProcessed := 0
    totalBuyersUploaded := 0
    totalSkipped := 0
    startTime := time.Now()

    client := meta.NewAudienceClient()

    for _, settings := range settingsList {
        var merchant domain.Merchant
        if err := pg.Where("id = ?", settings.MerchantID).First(&merchant).Error; err != nil {
            slog.Error("audience_sync: failed to fetch merchant store name, skipping merchant", "merchant_id", settings.MerchantID, "error", err)
            continue
        }

        // a. Query to find high-trust verified buyers for this merchant
        // SYNC WITH: internal/service/redis_trust.go EvaluateRisk
        query := `
            WITH weighted_stats AS (
                SELECT 
                    o.buyer_phone_normalized,
                    SUM(
                        CASE 
                            WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 6 THEN 1.0
                            WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 12 THEN 0.8
                            WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 24 THEN 0.5
                            ELSE 0.3
                        END
                    ) as w_total,
                    SUM(
                        CASE WHEN o.outcome = 'RTO' OR o.fulfillment_status = 'rto' THEN
                            CASE 
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 6 THEN 1.0
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 12 THEN 0.8
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 24 THEN 0.5
                                ELSE 0.3
                            END
                        ELSE 0.0 END
                    ) as w_rto,
                    SUM(
                        CASE WHEN o.outcome = 'CANCELLED' OR o.fulfillment_status = 'cancelled' THEN
                            CASE 
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 6 THEN 1.0
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 12 THEN 0.8
                                WHEN EXTRACT(YEAR FROM age(NOW(), o.created_at)) * 12 + EXTRACT(MONTH FROM age(NOW(), o.created_at)) <= 24 THEN 0.5
                                ELSE 0.3
                            END
                        ELSE 0.0 END
                    ) as w_cancel
                FROM orders o
                GROUP BY o.buyer_phone_normalized
            )
            SELECT DISTINCT be.phone_hash_meta
            FROM billable_events be
            INNER JOIN trust_profiles tp ON tp.phone_hash = be.phone_hash
            LEFT JOIN weighted_stats ws ON ws.buyer_phone_normalized = be.phone_hash
            WHERE be.merchant_id = ?
              AND be.created_at >= NOW() - INTERVAL '90 days'
              AND be.is_billable = true
              AND be.phone_hash_meta != ''
              AND tp.is_blacklisted = false
              AND COALESCE(ws.w_rto / NULLIF(ws.w_total, 0), tp.total_rtos::float / NULLIF(tp.total_orders, 0), 0) < 0.10
              AND COALESCE(ws.w_total, tp.total_orders::float, 0) >= 3
              AND (
                    (100.0
                      - COALESCE(ws.w_rto / NULLIF(ws.w_total, 0), tp.total_rtos::float / NULLIF(tp.total_orders, 0), 0) * 60
                      - COALESCE(ws.w_cancel / NULLIF(ws.w_total, 0), tp.total_cancellations::float / NULLIF(tp.total_orders, 0), 0) * 20
                      + tp.risk_adjustment)
                     >= 75.0
                  )
        `

        var results []string
        if err := pg.Raw(query, settings.MerchantID).Scan(&results).Error; err != nil {
            slog.Error("audience_sync: failed query for verified buyers", "merchant_id", settings.MerchantID, "error", err)
            continue
        }

        // b. If len(results) < 50: log and skip
        if len(results) < 50 {
            slog.Info(fmt.Sprintf("merchant %s: only %d verified buyers, skipping (minimum 50)", settings.MerchantID, len(results)))
            totalSkipped++
            continue
        }

        // c. Call UploadVerifiedBuyers
        audienceName := "Kaughtman Verified Buyers - " + merchant.StoreName
        res, err := client.UploadVerifiedBuyers(ctx, settings.MetaAdAccountID, settings.MetaAccessToken, audienceName, results)
        if err != nil {
            slog.Error("audience_sync: upload to Meta failed for merchant",
                "merchant_id", settings.MerchantID,
                "store_name", merchant.StoreName,
                "error", err,
            )
            continue
        }

        // d. Log result
        slog.Info("audience_sync: successfully uploaded custom audience for merchant",
            "merchant_id", settings.MerchantID,
            "store_name", merchant.StoreName,
            "buyers_uploaded", res.NumUsersAdded,
            "buyers_rejected", res.NumUsersRejected,
            "audience_id", res.AudienceID,
        )

        totalMerchantsProcessed++
        totalBuyersUploaded += res.NumUsersAdded
    }

    slog.Info("audience_sync: weekly run complete",
        "total_merchants_processed", totalMerchantsProcessed,
        "total_buyers_uploaded", totalBuyersUploaded,
        "total_skipped", totalSkipped,
        "runtime_seconds", time.Since(startTime).Seconds(),
    )

    return nil
}
