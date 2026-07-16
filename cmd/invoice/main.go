
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func lastCalendarMonth() string {
	now := time.Now()
	// Safely transition back 1 month using mid-month reference to avoid day overflow
	lastMonthTime := time.Date(now.Year(), now.Month(), 15, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
	return lastMonthTime.Format("2006-01")
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	slog.Info("starting scheduled monthly invoice generation job")

	pg := database.NewPostgresClient()

	billingMonth := lastCalendarMonth()
	if envMonth := os.Getenv("BILLING_MONTH"); envMonth != "" {
		billingMonth = envMonth
	}

	slog.Info("targeting billing month", "month", billingMonth)

	// 1. Find all MerchantBillingAccumulator rows where billing_month = last calendar month AND is_invoiced = false AND total_fee_paise > 0
	var accumulators []domain.MerchantBillingAccumulator
	err := pg.Where("billing_month = ? AND is_invoiced = ? AND total_fee_paise > 0", billingMonth, false).Find(&accumulators).Error
	if err != nil {
		slog.Error("failed to query billing accumulators", "error", err)
		os.Exit(1)
	}

	slog.Info("retrieved non-invoiced accumulators", "count", len(accumulators))

	invoicesCreated := 0
	totalAmountBilled := 0
	now := time.Now()

	parsedYear, parsedMonth, err := parseBillingMonth(billingMonth)
	if err != nil {
		slog.Error("failed to parse billing month format", "error", err)
		os.Exit(1)
	}

	billingPeriodStart := time.Date(parsedYear, parsedMonth, 1, 0, 0, 0, 0, time.UTC)
	billingPeriodEnd := billingPeriodStart.AddDate(0, 1, 0).Add(-time.Second)

	for _, acc := range accumulators {
		err := pg.Transaction(func(tx *gorm.DB) error {
			// Set strict transaction isolation level to completely isolate concurrent updates
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("SET TRANSACTION ISOLATION LEVEL SERIALIZABLE").Error; err != nil {
					return err
				}
			}

			// Lock the accumulator row for update to prevent concurrent updates
			var lockAcc domain.MerchantBillingAccumulator
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ?", acc.ID).First(&lockAcc).Error; err != nil {
				return err
			}

			if lockAcc.IsInvoiced {
				return fmt.Errorf("accumulator already invoiced")
			}
			// Query merchant to check active mode status
			var merchant domain.Merchant
			if err := tx.Where("id = ?", acc.MerchantID).First(&merchant).Error; err != nil {
				return err
			}

			if (merchant.Tier == "" || merchant.Tier == domain.TierFree) && !merchant.HasRTOEngine {
				// If on free tier with no active subscription/engine, skip invoicing
				res := tx.Model(&lockAcc).Update("is_invoiced", true)
				if res.Error != nil {
					return res.Error
				}
				slog.Info("skipped invoicing for free-tier merchant with no active paid subscription or RTO engine", "merchant_id", acc.MerchantID)
				return nil
			}

			// Compute sequence number
			var count int64
			err := tx.Model(&domain.MerchantInvoice{}).Where("merchant_id = ?", acc.MerchantID).Count(&count).Error
			if err != nil {
				return err
			}
			sequence := int(count) + 1

			shortMerchantID := acc.MerchantID
			if len(shortMerchantID) > 8 {
				shortMerchantID = shortMerchantID[:8]
			}
			formattedMonth := strings.ReplaceAll(billingMonth, "-", "")
			invoiceNumber := fmt.Sprintf("KTM-%s-%s-%04d", shortMerchantID, formattedMonth, sequence)

			// Calculate fees based on new pricing structure
			var finalFeePaise int = 0
			var notesParts []string

			// 1. Subscription Fee based on current tier
			switch merchant.Tier {
			case domain.TierGrowth:
				finalFeePaise += domain.GrowthMonthlyPaise // Rs. 6,999
				notesParts = append(notesParts, "Growth Subscription Fee: ₹6,999")
			case domain.TierGrowthAds:
				finalFeePaise += domain.GrowthAdsMonthlyPaise // Rs. 8,999
				notesParts = append(notesParts, "Growth + Ads Subscription Fee: ₹8,999")
			}

			// Calculate WhatsApp message surcharge (excess over ₹2,000 / 200,000 paise) for growth tiers
			if domain.IsGrowthOrAbove(merchant.Tier) {
				var totalWhatsAppCost int64 = 0
				tx.Model(&domain.WhatsAppMessageLog{}).
					Where("merchant_id = ? AND sent_at >= ? AND sent_at <= ?", merchant.ID, billingPeriodStart, billingPeriodEnd).
					Select("COALESCE(SUM(cost_paise), 0)").
					Row().Scan(&totalWhatsAppCost)

				// WhatsApp surcharge is fully waived if they also have active RTO Engine
				if merchant.HasRTOEngine {
					if totalWhatsAppCost > 0 {
						notesParts = append(notesParts, fmt.Sprintf("WhatsApp cost fully covered & waived (RTO Engine active): ₹%.2f", float64(totalWhatsAppCost)/100.0))
					}
				} else {
					const capPaise = domain.WhatsAppMonthlyCapPaise
					if totalWhatsAppCost > capPaise {
						excessPaise := totalWhatsAppCost - capPaise
						finalFeePaise += int(excessPaise)
						notesParts = append(notesParts, fmt.Sprintf("WhatsApp Surcharge: ₹%.2f (exceeded ₹2,000 threshold)", float64(excessPaise)/100.0))
					} else if totalWhatsAppCost > 0 {
						notesParts = append(notesParts, fmt.Sprintf("WhatsApp cost covered: ₹%.2f", float64(totalWhatsAppCost)/100.0))
					}
				}

				// If subscription was cancelled (meaning HasPaidSubscription is false), downgrade to free now at the end of the billing cycle
				if !merchant.HasPaidSubscription {
					if err := tx.Model(&merchant).Updates(map[string]interface{}{
						"tier":                    domain.TierFree,
						"subscription_renews_at":   nil,
						"subscription_started_at":  nil,
					}).Error; err != nil {
						return err
					}
					slog.Info("merchant tier downgraded to free at end of cycle", "merchant_id", merchant.ID)
				}
			}

			// 2. RTO Engine Transaction Fee (Pay-per-use)
			if merchant.HasRTOEngine {
				finalFeePaise += acc.TotalFeePaise
				notesParts = append(notesParts, fmt.Sprintf("RTO Engine transaction fees: ₹%.2f", float64(acc.TotalFeePaise)/100.0))
			}

			notesStr := strings.Join(notesParts, " | ")

			// 2.a. Create MerchantInvoice
			invoice := domain.MerchantInvoice{
				MerchantID:         acc.MerchantID,
				InvoiceNumber:      invoiceNumber,
				BillingPeriodStart: billingPeriodStart,
				BillingPeriodEnd:   billingPeriodEnd,
				TotalEventCount:    acc.TotalEvents,
				TotalFeePaise:      finalFeePaise,
				Status:             "pending",
				SentAt:             nil,
				PaidAt:             nil,
				RazorpayOrderID:    "",
				Notes:              notesStr,
			}

			if err := tx.Create(&invoice).Error; err != nil {
				return err
			}

			// 2.b. Update all BillableEvent rows for this merchant + month: set invoice_id = invoice.ID, billed_at = now()
			invoiceIDStr := fmt.Sprintf("%d", invoice.ID)
			err = tx.Model(&domain.BillableEvent{}).
				Where("merchant_id = ? AND created_at >= ? AND created_at <= ? AND (invoice_id = ? OR invoice_id = '')",
					acc.MerchantID, billingPeriodStart, billingPeriodEnd, "").
				Updates(map[string]interface{}{
					"invoice_id": invoiceIDStr,
					"billed_at":  &now,
				}).Error
			if err != nil {
				return err
			}

			// 2.c. Set accumulator.is_invoiced = true
			res := tx.Model(&lockAcc).Update("is_invoiced", true)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("accumulator update affected zero rows — potential race condition")
			}

			invoicesCreated++
			totalAmountBilled += invoice.TotalFeePaise
			return nil
		})

		if err != nil {
			slog.Error("failed generating invoice for merchant", "merchant_id", acc.MerchantID, "error", err)
			continue
		}
	}

	slog.Info("completed monthly invoice generation job",
		"invoices_created", invoicesCreated,
		"total_amount_billed_paise", totalAmountBilled,
		"month", billingMonth,
	)
}

func parseBillingMonth(monthStr string) (int, time.Month, error) {
	var year int
	var month int
	_, err := fmt.Sscanf(monthStr, "%d-%d", &year, &month)
	if err != nil {
		return 0, 0, err
	}
	return year, time.Month(month), nil
}
