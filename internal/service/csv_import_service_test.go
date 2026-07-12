package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/arpitmandhotra/api-integrator/internal/csvimport"
	"github.com/redis/go-redis/v9"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// mockRedisClient implements the minimum methods of the RedisClient interface
type mockRedisClient struct {
	store map[string][]byte
}

func newMockRedisClient() *mockRedisClient {
	return &mockRedisClient{store: make(map[string][]byte)}
}

func (m *mockRedisClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	var bytes []byte
	var err error
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		bytes, err = json.Marshal(value)
		if err != nil {
			cmd.SetErr(err)
			return cmd
		}
	}
	m.store[key] = bytes
	cmd.SetVal("OK")
	return cmd
}

func (m *mockRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	bytes, ok := m.store[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(string(bytes))
	return cmd
}

func (m *mockRedisClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	count := int64(0)
	for _, key := range keys {
		if _, ok := m.store[key]; ok {
			delete(m.store, key)
			count++
		}
	}
	cmd.SetVal(count)
	return cmd
}

func TestCSVImportPipeline_ValidateAndCommit(t *testing.T) {
	ctx := context.Background()

	// 1. Setup in-memory SQLite database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}

	// Migrate schema
	err = db.AutoMigrate(&domain.TrustProfile{})
	if err != nil {
		t.Fatalf("failed to migrate schema: %v", err)
	}

	// 2. Setup mock Redis client
	rdb := newMockRedisClient()

	// 3. Initialize CSVImportService
	svc := NewCSVImportService(db, rdb)

	// 4. Test validation stage with Shopify format and messy data
	// Shopify headers: Name (order_id), Phone (phone), Total (order_value), Created at (order_date), Fulfillment Status (order_status), Financial Status (financial_status)
	csvData := `Name,Phone,Total,Created at,Fulfillment Status,Financial Status
ORD1,+919876543210,$10.50,2026-01-02T15:04:05Z,fulfilled,paid
ORD2,,$5.00,2026-01-02T15:04:05Z,fulfilled,paid
ORD3,9876543211,"₹1,500.00",15/03/2024,restocked,refunded
ORD4,9876543212,Rs. 250.75,03/15/2024,restocked,voided
ORD5,9876543210,invalid_amount,2026-01-02T15:04:05Z,fulfilled,paid
ORD6,9876543210,$50.00,invalid_date,fulfilled,paid
ORD7,9876543210,$50.00,2026-01-02T15:04:05Z,unrecognized_status,paid
ORD3,9876543211,"₹1,500.00",16/03/2024,fulfilled,paid
`
	// Note on ORD3 duplicate:
	// - First row: ORD3, 9876543211, refund/restocked (rto)
	// - Second row: ORD3, 9876543211, fulfilled (fulfilled)
	// Since rto (3) > fulfilled (2) in status lifecycle, we keep the first row (rto) and discard the second row (fulfilled) as duplicate.

	report, err := svc.ValidateAndStage(ctx, strings.NewReader(csvData), "shopify")
	if err != nil {
		t.Fatalf("ValidateAndStage failed: %v", err)
	}

	// Verify report fields
	if report.TotalRows != 8 {
		t.Errorf("expected 8 total rows, got %d", report.TotalRows)
	}
	if report.RejectionReasons.InvalidPhone != 1 { // ORD2 has no phone
		t.Errorf("expected 1 invalid phone rejection, got %d", report.RejectionReasons.InvalidPhone)
	}
	if report.RejectionReasons.UnparseableAmount != 1 { // ORD5 has invalid_amount
		t.Errorf("expected 1 unparseable amount rejection, got %d", report.RejectionReasons.UnparseableAmount)
	}
	if report.RejectionReasons.UnparseableDate != 1 { // ORD6 has invalid_date
		t.Errorf("expected 1 unparseable date rejection, got %d", report.RejectionReasons.UnparseableDate)
	}
	if report.RejectionReasons.UnrecognizedStatus != 1 { // ORD7 has unrecognized_status
		t.Errorf("expected 1 unrecognized status rejection, got %d", report.RejectionReasons.UnrecognizedStatus)
	}
	if report.RejectionReasons.DuplicateOrderID != 1 { // ORD3 appears twice
		t.Errorf("expected 1 duplicate order ID rejection, got %d", report.RejectionReasons.DuplicateOrderID)
	}
	if report.AcceptedRows != 3 { // ORD1, ORD3 (RTO version), ORD4
		t.Errorf("expected 3 accepted rows, got %d", report.AcceptedRows)
	}
	if report.PreviewToken == "" {
		t.Errorf("expected non-empty preview token")
	}

	// Verify preview data is staged in Redis
	redisKey := "csvimport:preview:" + report.PreviewToken
	if _, ok := rdb.store[redisKey]; !ok {
		t.Errorf("preview data not staged in Redis under key %s", redisKey)
	}

	// Let's seed ORD1's phone hash (+919876543210)
	// Normalisation yields "919876543210"
	// Wait, crypto.HashPhone will actually hash the normalized digits "919876543210" with KAUGHTMAN_GLOBAL_PEPPER.
	// Since we are running the test with KAUGHTMAN_GLOBAL_PEPPER env variable set or defaulted, let's use the actual HashPhone value.
	t.Log("PEPPER in test:", svc.pg)

	// Let's resolve the phone hash for ORD1 (+919876543210)
	importPhoneHash, err := svc.csvSvcPhoneHash("+919876543210")
	if err != nil {
		t.Fatalf("failed to hash phone: %v", err)
	}

	// Insert pre-existing TrustProfile
	existingProfile := domain.TrustProfile{
		PhoneHash:            importPhoneHash,
		FirstSeenMerchantID:  "merchant_preexisting",
		TotalOrders:          5,
		SuccessfulDeliveries: 3,
		TotalRTOs:            2,
		TotalCancellations:   0,
		TotalRevenueSpent:    35050, // in paise (350.50 Rupees)
	}
	if err := db.Create(&existingProfile).Error; err != nil {
		t.Fatalf("failed to seed trust profile: %v", err)
	}

	// 5. Test commit stage
	commitRes, err := svc.Commit(ctx, report.PreviewToken, "merchant_current", "shopify")
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	// We expect 3 accepted rows:
	// - ORD1 (phone hash: +919876543210, value $10.50, status fulfilled). This profile is UPDATED.
	// - ORD3 (phone hash: 9876543211, value 1500.00, status RTO). This profile is CREATED.
	// - ORD4 (phone hash: 9876543212, value 250.75, status RTO-voided, which is a cancellation). This profile is CREATED.
	// So 1 updated profile, 2 created profiles.
	if commitRes.CreatedProfiles != 2 {
		t.Errorf("expected 2 created profiles, got %d", commitRes.CreatedProfiles)
	}
	if commitRes.UpdatedProfiles != 1 {
		t.Errorf("expected 1 updated profile, got %d", commitRes.UpdatedProfiles)
	}

	// Verify staged data is deleted from Redis
	if _, ok := rdb.store[redisKey]; ok {
		t.Errorf("staged preview data was not deleted from Redis after commit")
	}

	// Verify updated DB profile
	var updatedProfile domain.TrustProfile
	if err := db.Where("phone_hash = ?", importPhoneHash).First(&updatedProfile).Error; err != nil {
		t.Fatalf("failed to query updated profile: %v", err)
	}

	// Existing values: TotalOrders=5, SuccessfulDeliveries=3, TotalRTOs=2, TotalRevenueSpent=35050 paise
	// Added from ORD1: TotalOrders += 1, SuccessfulDeliveries += 1, TotalRevenueSpent += 1050 paise
	// Final expected: TotalOrders=6, SuccessfulDeliveries=4, TotalRTOs=2, TotalRevenueSpent=36100 paise
	if updatedProfile.TotalOrders != 6 {
		t.Errorf("expected TotalOrders=6, got %d", updatedProfile.TotalOrders)
	}
	if updatedProfile.SuccessfulDeliveries != 4 {
		t.Errorf("expected SuccessfulDeliveries=4, got %d", updatedProfile.SuccessfulDeliveries)
	}
	if updatedProfile.TotalRTOs != 2 {
		t.Errorf("expected TotalRTOs=2, got %d", updatedProfile.TotalRTOs)
	}
	if updatedProfile.TotalRevenueSpent != 36100 {
		t.Errorf("expected TotalRevenueSpent=36100, got %d", updatedProfile.TotalRevenueSpent)
	}
}

// Helper to get phone hash during tests
func (s *CSVImportService) csvSvcPhoneHash(phone string) (string, error) {
	// The normalization hashes the number. We can use csvimport.ParsePhone
	return csvimport.ParsePhone(phone)
}
