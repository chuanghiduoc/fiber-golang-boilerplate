package dto

// PaginationQuery is offset/page-based pagination. Prefer CursorQuery for
// high-volume list endpoints; use this only for small, bounded tables.
type PaginationQuery struct {
	Page    int `query:"page"`
	PerPage int `query:"pageSize"`
}

// CursorQuery is forward-only cursor pagination (the default for list
// endpoints). startingAfter is an opaque cursor from the last item of the
// previous page; omit it for the first page.
type CursorQuery struct {
	Limit         int    `query:"limit"`
	StartingAfter string `query:"startingAfter"`
}
