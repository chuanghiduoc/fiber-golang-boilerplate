package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
)

func TestToFileResponse(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	live := filesvc.File{ID: 9, TenantID: 2, UserID: 3, OriginalName: "a.pdf", StoragePath: "2/3/x.pdf", MimeType: "application/pdf", Size: 42, CreatedAt: created}

	data, err := json.Marshal(toFileResponse(&live))
	require.NoError(t, err)
	// The storage path is never exposed; a live file carries its download URL.
	assert.JSONEq(t, `{"id":9,"tenantId":2,"userId":3,"originalName":"a.pdf","mimeType":"application/pdf","size":42,"url":"/api/v1/files/9/download","createdAt":"2026-01-02T03:04:05Z"}`, string(data))

	deleted := live
	deleted.DeletedAt = &created
	resp := toFileResponse(&deleted)
	assert.Empty(t, resp.URL, "a deleted file must not expose a download URL")
	assert.Equal(t, &created, resp.DeletedAt)
}
