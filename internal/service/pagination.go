package service

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// buildCursor normalizes the page size and decodes an optional startingAfter
// cursor into a repository.Cursor. The returned pageSize is the caller-visible
// limit; the repository fetches pageSize+1 rows so callers can detect hasMore.
func buildCursor(limit int, startingAfter string) (cur repository.Cursor, pageSize int, err error) {
	pageSize = pagination.NormalizeLimit(limit)
	cur = repository.Cursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, decErr := pagination.DecodeCursor(startingAfter)
		if decErr != nil {
			return repository.Cursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}
