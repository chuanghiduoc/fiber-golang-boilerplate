package pagination

import "math"

const (
	DefaultPage    = 1
	DefaultPerPage = 10
	MaxPerPage     = 100
)

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
// It normalizes inputs before conversion so callers don't need to call Normalize first.
func LimitOffset(page, perPage int) (limit, offset int32) {
	page, perPage = Normalize(page, perPage)

	off := (page - 1) * perPage

	if perPage > math.MaxInt32 {
		perPage = math.MaxInt32
	}
	if off > math.MaxInt32 {
		off = math.MaxInt32
	}
	limit = int32(perPage)
	offset = int32(off)
	return limit, offset
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
