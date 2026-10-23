package pagination

import "math"

const (
	DefaultPage    = 1
	DefaultPerPage = 10
	MaxPerPage     = 100
)

// clampInt32 safely converts int to int32 with clamping.
// This is the single place where int→int32 conversion is suppressed,
// keeping gosec G115 enabled globally to catch unsafe casts elsewhere.
func clampInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v) // #nosec G115 -- bounds checked above
}

// Normalize clamps page and perPage to valid ranges.
func Normalize(page, perPage int) (normalizedPage, normalizedPerPage int) {
	if page < 1 {
		page = DefaultPage
	}
	if perPage < 1 {
		perPage = DefaultPerPage
	}
	if perPage > MaxPerPage {
		perPage = MaxPerPage
	}
	return page, perPage
}

// LimitOffset returns safe int32 limit and offset for SQL queries.
func LimitOffset(page, perPage int) (limit, offset int32) {
	page, perPage = Normalize(page, perPage)
	if page > math.MaxInt32 {
		page = math.MaxInt32
	}
	off := int64(page-1) * int64(perPage)
	if off > math.MaxInt32 {
		off = math.MaxInt32
	}
	return clampInt32(perPage), clampInt32(int(off))
}

// TotalPages calculates total number of pages.
func TotalPages(total int64, perPage int) int {
	if perPage <= 0 {
		return 0
	}
	tp := int(total) / perPage
	if int(total)%perPage != 0 {
		tp++
	}
	return tp
}
