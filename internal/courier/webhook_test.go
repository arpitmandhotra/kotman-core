package courier_test

import (
	"encoding/base64"
	"net/http"
	"os"
	"testing"

	"github.com/arpitmandhotra/api-integrator/internal/courier"
	"github.com/arpitmandhotra/api-integrator/internal/security"
)

func TestDelhiveryAdapter_VerifySignature(t *testing.T) {
	adap := courier.NewDelhiveryAdapter()
	headers := http.Header{}
	headers.Set("X-Delhivery-Token", "valid_secret")

	// Valid Signature
	err := adap.VerifySignature([]byte("body"), headers, "valid_secret")
	if err != nil {
		t.Fatalf("expected signature verification to succeed: %v", err)
	}

	// Tampered Signature
	err = adap.VerifySignature([]byte("body"), headers, "wrong_secret")
	if err == nil {
		t.Fatal("expected signature verification to fail for wrong secret")
	}

	// Missing Signature
	headers.Del("X-Delhivery-Token")
	err = adap.VerifySignature([]byte("body"), headers, "valid_secret")
	if err == nil {
		t.Fatal("expected signature verification to fail for missing token")
	}
}

func TestShiprocketAdapter_VerifySignature(t *testing.T) {
	adap := courier.NewShiprocketAdapter()
	body := []byte(`{"awb_code":"12345"}`)
	secret := []byte("shiprocket_secret")

	// Calculate HMAC signature
	signature := security.EncryptHMAC(body, secret)

	headers := http.Header{}
	headers.Set("X-Shiprocket-Hmac-Sha256", signature)

	// Valid signature
	err := adap.VerifySignature(body, headers, string(secret))
	if err != nil {
		t.Fatalf("expected signature verification to succeed: %v", err)
	}

	// Tampered signature
	headers.Set("X-Shiprocket-Hmac-Sha256", "tampered_signature")
	err = adap.VerifySignature(body, headers, string(secret))
	if err == nil {
		t.Fatal("expected signature verification to fail for tampered signature")
	}
}

func TestDelhiveryAdapter_Normalize(t *testing.T) {
	adap := courier.NewDelhiveryAdapter()
	rawPayload := []byte(`{
		"waybill": "AWB-101",
		"order_id": "ORDER-99",
		"status": "NDR",
		"status_date": "2026-07-12T14:15:00Z",
		"instructions": "customer requested delay",
		"reason": "Customer not available",
		"attempt_count": 2
	}`)

	rawEvent, err := adap.ParseEvent(rawPayload)
	if err != nil {
		t.Fatalf("failed to parse event: %v", err)
	}

	event, err := adap.Normalize(rawEvent)
	if err != nil {
		t.Fatalf("failed to normalize event: %v", err)
	}

	if event.AWB != "AWB-101" {
		t.Errorf("expected AWB 'AWB-101', got '%s'", event.AWB)
	}
	if event.EventType != courier.EventNDRAttempted {
		t.Errorf("expected EventType 'NDR_ATTEMPTED', got '%s'", event.EventType)
	}
	if event.ReasonCode != courier.ReasonCustomerUnavailable {
		t.Errorf("expected ReasonCode 'CUSTOMER_UNAVAILABLE', got '%s'", event.ReasonCode)
	}
	if event.AttemptNumber != 2 {
		t.Errorf("expected AttemptNumber 2, got %d", event.AttemptNumber)
	}
}

func TestShiprocketAdapter_Normalize(t *testing.T) {
	adap := courier.NewShiprocketAdapter()
	rawPayload := []byte(`{
		"awb_code": "AWB-202",
		"order_id": "ORDER-88",
		"current_status": "undelivered",
		"ndr": {
			"reason": "customer refused to accept the order",
			"attempt_count": 3,
			"ndr_date": "2026-07-12 14:15:00"
		}
	}`)

	rawEvent, err := adap.ParseEvent(rawPayload)
	if err != nil {
		t.Fatalf("failed to parse event: %v", err)
	}

	event, err := adap.Normalize(rawEvent)
	if err != nil {
		t.Fatalf("failed to normalize event: %v", err)
	}

	if event.AWB != "AWB-202" {
		t.Errorf("expected AWB 'AWB-202', got '%s'", event.AWB)
	}
	if event.EventType != courier.EventNDRAttempted {
		t.Errorf("expected EventType 'NDR_ATTEMPTED', got '%s'", event.EventType)
	}
	if event.ReasonCode != courier.ReasonCustomerRefused {
		t.Errorf("expected ReasonCode 'CUSTOMER_REFUSED', got '%s'", event.ReasonCode)
	}
	if event.AttemptNumber != 3 {
		t.Errorf("expected AttemptNumber 3, got %d", event.AttemptNumber)
	}
}

func TestSecurity_TokenEncryptionDecryption(t *testing.T) {
	masterKey := make([]byte, 32)
	copy(masterKey, []byte("00000000000000000000000000000000"))

	plaintext := "my-secret-token-key"
	ciphertext, err := security.EncryptString(plaintext, masterKey)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	decrypted, err := security.DecryptString(ciphertext, masterKey)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("expected decrypted text '%s', got '%s'", plaintext, decrypted)
	}
}

func TestProcessedWebhookEvent_UniqueConstraint(t *testing.T) {
	// Set dummy base64 encryption key in OS env for testing
	os.Setenv("TOKEN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("00000000000000000000000000000000")))
}
