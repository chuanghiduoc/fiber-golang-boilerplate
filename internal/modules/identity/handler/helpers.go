package handler

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// nextUserCursor exposes the opaque cursor clients must pass as
// startingAfter on the next request.
func nextUserCursor(items []dto.UserResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}
