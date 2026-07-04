package handlers

import (
	"testing"
)

func TestPercentileFromSorted(t *testing.T) {
	// Case 1: Empty slice
	if val := percentileFromSorted([]float64{}, 90); val != 0 {
		t.Errorf("expected 0 for empty slice, got %f", val)
	}

	// Case 2: Single element slice (no panic, returns element)
	if val := percentileFromSorted([]float64{500.0}, 90); val != 500.0 {
		t.Errorf("expected 500.0 for single element, got %f", val)
	}

	// Case 3: Multiple elements (interpolated check)
	sorted := []float64{100.0, 200.0, 300.0, 400.0, 500.0}
	
	// Median (50th percentile) -> should be 300.0
	if val := percentileFromSorted(sorted, 50); val != 300.0 {
		t.Errorf("expected 300.0 for 50th percentile, got %f", val)
	}

	// 25th percentile -> idx = 0.25 * 4 = 1.0 -> 200.0
	if val := percentileFromSorted(sorted, 25); val != 200.0 {
		t.Errorf("expected 200.0 for 25th percentile, got %f", val)
	}

	// 90th percentile -> idx = 0.9 * 4 = 3.6 -> lower = 3, upper = 4 -> 400.0 + 0.6 * (500.0 - 400.0) = 460.0
	if val := percentileFromSorted(sorted, 90); val != 460.0 {
		t.Errorf("expected 460.0 for 90th percentile, got %f", val)
	}
}
