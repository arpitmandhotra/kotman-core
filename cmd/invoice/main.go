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

			// 2.a. Create MerchantInvoice
			invoice := domain.MerchantInvoice{
				MerchantID:         acc.MerchantID,
				InvoiceNumber:      invoiceNumber,
				BillingPeriodStart: billingPeriodStart,
				BillingPeriodEnd:   billingPeriodEnd,
				TotalEventCount:    acc.TotalEvents,
				TotalFeePaise:      acc.TotalFeePaise,
				Status:             "pending",
				SentAt:             nil,
				PaidAt:             nil,
				RazorpayOrderID:    "",
				Notes:              fmt.Sprintf("Automatically generated invoice for period %s", billingMonth),
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
			totalAmountBilled += acc.TotalFeePaise
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
