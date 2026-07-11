package main

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/testcontainers/testcontainers-go"
    tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
    "github.com/testcontainers/testcontainers-go/wait"
    postgres_driver "gorm.io/driver/postgres"
    "gorm.io/gorm"

    "github.com/arpitmandhotra/api-integrator/internal/crypto"
    "github.com/arpitmandhotra/api-integrator/internal/domain"
)

func TestRunAudienceSync_Integration(t *testing.T) {
    ctx := context.Background()

    t.Log("Orchestrating isolated Postgres container...")

    pgContainer, err := tcpostgres.RunContainer(ctx,
        testcontainers.WithImage("postgres:15-alpine"),
        tcpostgres.WithDatabase("kotman_test_audience"),
        tcpostgres.WithUsername("testuser"),
        tcpostgres.WithPassword("testpass"),
        testcontainers.WithWaitStrategy(
            wait.ForLog("database system is ready to accept connections").
                WithOccurrence(2).
                WithStartupTimeout(15*time.Second),
        ),
    )
    if err != nil {
        t.Skipf("Skipping integration test: Docker/Testcontainers not available: %s", err)
    }
    defer func() {
        if err := pgContainer.Terminate(ctx); err != nil {
            t.Logf("warning: failed to terminate Postgres container: %s", err)
        }
    }()

    pgURI, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
    if err != nil {
        t.Fatalf("Could not get Postgres connection string: %s", err)
    }

    pgDB, err := gorm.Open(postgres_driver.Open(pgURI), &gorm.Config{})
    if err != nil {
        t.Fatalf("failed to connect to Postgres: %s", err)
    }

    if err := pgDB.AutoMigrate(
        &domain.Merchant{},
        &domain.MerchantSettings{},
        &domain.BillableEvent{},
        &domain.TrustProfile{},
    ); err != nil {
        t.Fatalf("failed to migrate database schema: %v", err)
    }

    merchantID := "test-meta-merchant"
    if err := pgDB.Create(&domain.Merchant{
        ID:         merchantID,
        StoreName:  "Test Meta Store",
        APIKeyHash: crypto.HashAPIKey("test_api_key_456"),
        IsActive:   true,
    }).Error; err != nil {
        t.Fatalf("failed to seed merchant: %v", err)
    }

    if err := pgDB.Create(&domain.MerchantSettings{
        MerchantID:         merchantID,
        MetaPixelID:        "pixel_123",
        MetaAccessToken:    "token_xyz",
        MetaAdAccountID:    "act_123",
        MetaCAPIEnabled:    true,
        WalletBalancePaise: 100000,
    }).Error; err != nil {
        t.Fatalf("failed to seed merchant settings: %v", err)
    }

    for i := 1; i <= 60; i++ {
        phone := fmt.Sprintf("+919000000%03d", i)
        phoneHash := crypto.HashPhone(phone)
        phoneHashMeta := fmt.Sprintf("meta_hash_%03d", i)

        tp := domain.TrustProfile{
            PhoneHash:      phoneHash,
            TotalOrders:    5,
            TotalRTOs:      0,
            RiskAdjustment: 0,
            IsBlacklisted:  false,
        }
        if err := pgDB.Create(&tp).Error; err != nil {
            t.Fatalf("failed to seed trust profile %d: %v", i, err)
        }

        be := domain.BillableEvent{
            MerchantID:    merchantID,
            OrderID:       fmt.Sprintf("ord_%03d", i),
            Platform:      "shopify",
            PhoneHash:     phoneHash,
            PhoneHashMeta: phoneHashMeta,
            IsBillable:    true,
        }
        if err := pgDB.Create(&be).Error; err != nil {
            t.Fatalf("failed to seed billable event %d: %v", i, err)
        }
    }

    for i := 61; i <= 80; i++ {
        phone := fmt.Sprintf("+919000000%03d", i)
        phoneHash := crypto.HashPhone(phone)
        phoneHashMeta := fmt.Sprintf("meta_hash_%03d", i)

        tp := domain.TrustProfile{
            PhoneHash:      phoneHash,
            TotalOrders:    1,
            TotalRTOs:      0,
            RiskAdjustment: 0,
            IsBlacklisted:  false,
        }
        if err := pgDB.Create(&tp).Error; err != nil {
            t.Fatalf("failed to seed trust profile %d: %v", i, err)
        }

        be := domain.BillableEvent{
            MerchantID:    merchantID,
            OrderID:       fmt.Sprintf("ord_%03d", i),
            Platform:      "shopify",
            PhoneHash:     phoneHash,
            PhoneHashMeta: phoneHashMeta,
            IsBillable:    true,
        }
        if err := pgDB.Create(&be).Error; err != nil {
            t.Fatalf("failed to seed billable event %d: %v", i, err)
        }
    }

    var receivedHashes []string
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")

        if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/customaudiences") {
            w.Write([]byte(`{"data": [{"id": "aud_12345", "name": "Kotman Verified Buyers - Test Meta Store"}]}`))
            return
        }

        if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/users") {
            bodyBytes, err := io.ReadAll(r.Body)
            if err != nil {
                t.Fatalf("failed to read body: %v", err)
            }

            var uploadPayload struct {
                Data [][]string `json:"data"`
            }
            if err := json.Unmarshal(bodyBytes, &uploadPayload); err != nil {
                t.Fatalf("failed to unmarshal upload payload: %v", err)
            }

            for _, row := range uploadPayload.Data {
                receivedHashes = append(receivedHashes, row[0])
            }

            w.Write([]byte(fmt.Sprintf(`{"num_received": %d, "num_invalid": 0}`, len(uploadPayload.Data))))
            return
        }

        w.WriteHeader(http.StatusNotFound)
    }))
    defer server.Close()

    os.Setenv("META_GRAPH_API_BASE", server.URL)
    defer os.Unsetenv("META_GRAPH_API_BASE")

    err = RunAudienceSync(pgDB)
    if err != nil {
        t.Fatalf("RunAudienceSync failed: %v", err)
    }

    if len(receivedHashes) != 60 {
        t.Errorf("expected mock server to receive exactly 60 hashes, got %d", len(receivedHashes))
    }

    for _, hash := range receivedHashes {
        if !strings.HasPrefix(hash, "meta_hash_") {
            t.Errorf("unexpected hash received: %s", hash)
        }
        numStr := strings.TrimPrefix(hash, "meta_hash_")
        var num int
        fmt.Sscanf(numStr, "%d", &num)
        if num < 1 || num > 60 {
            t.Errorf("received out of range hash: %s", hash)
        }
    }
}
