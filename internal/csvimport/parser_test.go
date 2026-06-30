package csvimport

import (
	"testing"
)

func TestParsePhone(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"+919876543210", false},
		{"9876543210", false},
		{"", true},
		{"abc", true},
		{"  ", true},
	}

	for _, tt := range tests {
		got, err := ParsePhone(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePhone(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
		if err == nil && got == "" {
			t.Errorf("ParsePhone(%q) returned empty hash on success", tt.input)
		}
	}
}

func TestParseAmount(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"$10.50", 1050, false},
		{"₹1,500.75", 150075, false},
		{"Rs. 500", 50000, false},
		{"100", 10000, false},
		{"-5.50", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseAmount(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseAmount(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"2026-01-02T15:04:05Z", false},
		{"2025-12-31", false},
		{"15/03/2024", false}, // DD/MM/YYYY
		{"03/15/2024", false}, // MM/DD/YYYY
		{"Jan 2, 2023", false},
		{"2009-12-31", true}, // too old
		{"2030-01-01", true}, // in the future
		{"invalid-date", true},
	}

	for _, tt := range tests {
		got, err := ParseDate(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseDate(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
		if err == nil && got.IsZero() {
			t.Errorf("ParseDate(%q) returned zero time on success", tt.input)
		}
	}
}

func TestMapOrderStatus(t *testing.T) {
	tests := []struct {
		platform string
		input    string
		want     string
	}{
		{"shopify", "fulfilled", "fulfilled"},
		{"shopify", "restocked:refunded", "rto"},
		{"shopify", "fulfilled:voided", "fulfilled"}, // fulfilled takes priority in MapOrderStatus
		{"woocommerce", "completed", "fulfilled"},
		{"woocommerce", "cancelled", "rto"},
		{"woocommerce", "processing", "order_created"},
		{"magento", "complete", "fulfilled"},
		{"magento", "closed", "rto"},
		{"generic", "Order Returned", "rto"}, // fuzzy free-text match
		{"generic", "unknown", "unrecognized"},
	}

	for _, tt := range tests {
		got, err := MapOrderStatus(tt.platform, tt.input)
		if err != nil {
			t.Errorf("MapOrderStatus(%s, %q) unexpected error = %v", tt.platform, tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("MapOrderStatus(%s, %q) = %q, want %q", tt.platform, tt.input, got, tt.want)
		}
	}
}
