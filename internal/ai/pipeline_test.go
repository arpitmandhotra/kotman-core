package ai

import (
	"strings"
	"testing"
)

func TestGenerateDeterministicInsight(t *testing.T) {
	tests := []struct {
		scoreType string
		score     float64
		contains  string
	}{
		// rto_efficiency
		{"rto_efficiency", 20, "Your RTO rate is critically high"},
		{"rto_efficiency", 50, "Your RTO rate is above the network average"},
		{"rto_efficiency", 70, "Your RTO efficiency is improving"},
		{"rto_efficiency", 90, "Your RTO efficiency is strong"},

		// buyer_quality
		{"buyer_quality", 20, "Your buyer base skews heavily toward at-risk"},
		{"buyer_quality", 50, "Your buyer quality is moderate"},
		{"buyer_quality", 70, "Your buyer quality is healthy"},
		{"buyer_quality", 90, "Your buyer quality is among the strongest"},

		// operations
		{"operations", 20, "Your operations score reflects significant gaps"},
		{"operations", 50, "Your operational efficiency is below network median"},
		{"operations", 70, "Your operations are functioning well"},
		{"operations", 90, "Your operations score is strong"},

		// default
		{"unknown", 50, "Review your score breakdown to identify the specific metric"},
	}

	for _, tt := range tests {
		t.Run(tt.scoreType+"_"+strings.ReplaceAll(tt.contains[:10], " ", "_"), func(t *testing.T) {
			res := GenerateDeterministicInsight(tt.scoreType, tt.score, nil)
			if !strings.Contains(res, tt.contains) {
				t.Errorf("expected insight to contain %q, got %q", tt.contains, res)
			}
		})
	}
}
