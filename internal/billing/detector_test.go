package billing

import (
	"testing"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

func TestDetectCheckoutMode(t *testing.T) {
	tests := []struct {
		name             string
		payload          OrderPayload
		merchantSettings domain.MerchantSettings
		expectedMode     string
		expectedTPName   string
	}{
		{
			name: "1. Shopify order with kaughtman cart attributes -> native",
			payload: OrderPayload{
				Platform: "shopify",
				NoteAttributes: map[string]string{
					"kaughtman_risk": "high",
					"kaughtman_ts":   "12345678",
				},
				SourceName: "web",
			},
			merchantSettings: domain.MerchantSettings{
				CheckoutMode: "third_party",
			},
			expectedMode:   "native",
			expectedTPName: "",
		},
		{
			name: "2. Shopify order with gokwik_order_id in note_attributes -> third_party/gokwik",
			payload: OrderPayload{
				Platform: "shopify",
				NoteAttributes: map[string]string{
					"gokwik_order_id": "gk_123",
				},
			},
			merchantSettings: domain.MerchantSettings{},
			expectedMode:     "third_party",
			expectedTPName:   "gokwik",
		},
		{
			name: "3. Shopify order with source_name = GoKwik -> third_party/gokwik",
			payload: OrderPayload{
				Platform:   "shopify",
				SourceName: "GoKwik",
			},
			merchantSettings: domain.MerchantSettings{},
			expectedMode:     "third_party",
			expectedTPName:   "gokwik",
		},
		{
			name: "4. WooCommerce order with _gokwik_source in meta_data -> third_party/gokwik",
			payload: OrderPayload{
				Platform: "woocommerce",
				NoteAttributes: map[string]string{
					"_gokwik_source": "gokwik",
				},
			},
			merchantSettings: domain.MerchantSettings{},
			expectedMode:     "third_party",
			expectedTPName:   "gokwik",
		},
		{
			name: "5. Magento order with no third-party fingerprints, merchant configured as native -> native",
			payload: OrderPayload{
				Platform:      "magento",
				PaymentMethod: "cod",
			},
			merchantSettings: domain.MerchantSettings{
				CheckoutMode: "native",
			},
			expectedMode:   "native",
			expectedTPName: "",
		},
		{
			name: "6. Order with BOTH kaughtman cart attribute AND GoKwik fingerprint -> native wins",
			payload: OrderPayload{
				Platform: "shopify",
				NoteAttributes: map[string]string{
					"kaughtman_risk":  "low",
					"gokwik_order_id": "gk_999",
				},
				SourceName: "GoKwik",
			},
			merchantSettings: domain.MerchantSettings{},
			expectedMode:     "native",
			expectedTPName:   "",
		},
		{
			name: "7. Unknown payment gateway on native checkout -> native",
			payload: OrderPayload{
				Platform:      "shopify",
				PaymentMethod: "unknown_gateway",
			},
			merchantSettings: domain.MerchantSettings{
				CheckoutMode: "native",
			},
			expectedMode:   "native",
			expectedTPName: "",
		},
		{
			name: "8. Third-party checkout prepaid order -> third_party (requires review/waived fee)",
			payload: OrderPayload{
				Platform:      "shopify",
				PaymentMethod: "prepaid",
				SourceName:    "shopflo",
			},
			merchantSettings: domain.MerchantSettings{
				CheckoutMode: "third_party",
			},
			expectedMode:   "third_party",
			expectedTPName: "shopflo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := DetectCheckoutMode(tt.payload, tt.merchantSettings)
			if res.Mode != tt.expectedMode {
				t.Errorf("expected Mode %q, got %q", tt.expectedMode, res.Mode)
			}
			if res.ThirdPartyName != tt.expectedTPName {
				t.Errorf("expected ThirdPartyName %q, got %q", tt.expectedTPName, res.ThirdPartyName)
			}
		})
	}
}

func TestDetectPaymentMethod(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		payload  map[string]interface{}
		expected string
	}{
		{
			name:     "Shopify COD manual",
			platform: "shopify",
			payload:  map[string]interface{}{"payment_gateway": "manual"},
			expected: "cod",
		},
		{
			name:     "Shopify prepaid Razorpay",
			platform: "shopify",
			payload:  map[string]interface{}{"payment_gateway": "razorpay_gateway"},
			expected: "prepaid",
		},
		{
			name:     "WooCommerce COD title",
			platform: "woocommerce",
			payload:  map[string]interface{}{"payment_method": "custom", "payment_method_title": "Cash On Delivery"},
			expected: "cod",
		},
		{
			name:     "WooCommerce status completed",
			platform: "woocommerce",
			payload:  map[string]interface{}{"payment_method": "stripe", "status": "completed"},
			expected: "prepaid",
		},
		{
			name:     "Magento COD cashondelivery",
			platform: "magento",
			payload:  map[string]interface{}{"payment": map[string]interface{}{"method": "cashondelivery"}},
			expected: "cod",
		},
		{
			name:     "Magento prepaid",
			platform: "magento",
			payload:  map[string]interface{}{"payment": map[string]interface{}{"method": "stripe"}},
			expected: "prepaid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := DetectPaymentMethod(tt.platform, tt.payload)
			if res != tt.expected {
				t.Errorf("expected payment method %q, got %q", tt.expected, res)
			}
		})
	}
}

func TestComputeFeeAndTiers(t *testing.T) {
	feeTests := []struct {
		value    int
		expected int
	}{
		{49999, 500},
		{50000, 500},
		{50001, 750},
		{100000, 750},
		{100001, 1500},
		{200000, 1500},
		{200001, 2000},
		{300000, 2000},
		{300001, 2500},
		{400000, 2500},
		{400001, 3000},
		{500000, 3000},
		{500001, 5000},
		{1000000, 5000},
		{1000001, 10000},
	}
	for _, tt := range feeTests {
		res := domain.KaughtmanFee(tt.value)
		if res != tt.expected {
			t.Errorf("KaughtmanFee(%d) expected %d, got %d", tt.value, tt.expected, res)
		}
	}

	// Third-party checkout prepaid order -> not billable, 0 fee
	isB, fee := ComputeFee("third_party", "prepaid", 150000)
	if isB || fee != 0 {
		t.Errorf("Expected third-party prepaid to be non-billable with 0 fee, got: isBillable=%t, fee=%d", isB, fee)
	}

	// Third-party checkout COD order -> billable, tiered fee
	isB, fee = ComputeFee("third_party", "cod", 150000)
	if !isB || fee != 1500 {
		t.Errorf("Expected third-party COD to be billable with 1500 paise fee, got: isBillable=%t, fee=%d", isB, fee)
	}
}
