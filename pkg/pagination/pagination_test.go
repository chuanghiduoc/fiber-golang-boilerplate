package pagination

import (
	"math"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name            string
		page, perPage   int
		wantPage, wantP int
	}{
		{"valid values", 2, 20, 2, 20},
		{"page < 1 defaults to 1", 0, 10, 1, 10},
		{"negative page defaults to 1", -5, 10, 1, 10},
		{"perPage < 1 defaults to 10", 1, 0, 1, DefaultPerPage},
		{"negative perPage defaults to 10", 1, -1, 1, DefaultPerPage},
		{"perPage > 100 clamped to 100", 1, 200, 1, MaxPerPage},
		{"both invalid", 0, 0, 1, DefaultPerPage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, pp := Normalize(tt.page, tt.perPage)
			if p != tt.wantPage || pp != tt.wantP {
				t.Errorf("Normalize(%d, %d) = (%d, %d), want (%d, %d)",
					tt.page, tt.perPage, p, pp, tt.wantPage, tt.wantP)
			}
		})
	}
}

func TestLimitOffset(t *testing.T) {
	tests := []struct {
		name             string
		page, perPage    int
		wantLim, wantOff int32
	}{
		{"page 1", 1, 10, 10, 0},
		{"page 2", 2, 10, 10, 10},
		{"page 3 per 5", 3, 5, 5, 10},
		{"invalid page normalizes", 0, 10, 10, 0},
		{"perPage clamped to max", 1, 200, 100, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim, off := LimitOffset(tt.page, tt.perPage)
			if lim != tt.wantLim || off != tt.wantOff {
				t.Errorf("LimitOffset(%d, %d) = (%d, %d), want (%d, %d)",
					tt.page, tt.perPage, lim, off, tt.wantLim, tt.wantOff)
			}
		})
	}
}

func TestTotalPages(t *testing.T) {
	tests := []struct {
		name    string
		total   int64
		perPage int
		want    int
	}{
		{"zero total", 0, 10, 0},
		{"exact division", 20, 10, 2},
		{"remainder rounds up", 25, 10, 3},
		{"single item", 1, 10, 1},
		{"perPage zero returns 0", 10, 0, 0},
		{"perPage negative returns 0", 10, -1, 0},
		{"large total", 1001, 100, 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TotalPages(tt.total, tt.perPage)
			if got != tt.want {
				t.Errorf("TotalPages(%d, %d) = %d, want %d",
					tt.total, tt.perPage, got, tt.want)
			}
		})
	}
}

func TestClampInt32(t *testing.T) {
	tests := []struct {
		name string
		v    int
		want int32
	}{
		{"normal value", 42, 42},
		{"zero", 0, 0},
		{"max int32", math.MaxInt32, math.MaxInt32},
		{"above max int32", math.MaxInt32 + 1, math.MaxInt32},
		{"min int32", math.MinInt32, math.MinInt32},
		{"below min int32", math.MinInt32 - 1, math.MinInt32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampInt32(tt.v)
			if got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}
