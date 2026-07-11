package backfill

import "testing"

func TestValidateIndianMobilePhone(t *testing.T) {
	tests := []struct {
		input   string
		wantOut string
		wantOK  bool
	}{
		// Valid numbers in various formats
		{"+919876012345", "9876012345", true},
		{"09876012345", "9876012345", true},
		{"919876012345", "9876012345", true},
		{"9876012345", "9876012345", true},
		{"98760 12345", "9876012345", true},
		{"98760-12345", "9876012345", true},

		// Valid starting digits (6, 7, 8)
		{"6123456789", "6123456789", true},
		{"7123456789", "7123456789", true},
		{"8123456789", "8123456789", true},

		// Landlines — start with digits < 6, reject
		{"02212345678", "", false}, // Mumbai landline with STD code
		{"01123456789", "", false}, // Delhi landline

		// Starts with 1–5 — reject
		{"5123456789", "", false},
		{"4012345678", "", false},

		// All same digit — reject
		{"9999999999", "", false},
		{"8888888888", "", false},
		{"7777777777", "", false},
		{"6666666666", "", false},
		{"0000000000", "", false},

		// Known garbage numbers — reject
		{"1234567890", "", false},
		{"9876543210", "", false},
		{"9000000000", "", false},
		{"8000000000", "", false},

		// Too short — reject
		{"98765", "", false},
		{"987654321", "", false},

		// Too long — reject
		{"9876543210123", "", false},
		{"919876543210123", "", false},

		// Empty / whitespace — reject
		{"", "", false},
		{" ", "", false},
	}

	for _, tt := range tests {
		got, ok := validateIndianMobilePhone(tt.input)
		if ok != tt.wantOK || got != tt.wantOut {
			t.Errorf("validateIndianMobilePhone(%q) = (%q, %v), want (%q, %v)",
				tt.input, got, ok, tt.wantOut, tt.wantOK)
		}
	}
}
