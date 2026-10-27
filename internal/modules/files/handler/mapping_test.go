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
	live := filesvc.File{ID: testID(9), TenantID: testID(2), UserID: testID(3), OriginalName: "a.pdf", StoragePath: "2/3/x.pdf", MimeType: "application/pdf", Size: 42, CreatedAt: created}

	data, err := json.Marshal(toFileResponse(&live))
	require.NoError(t, err)
	// The storage path is never exposed; a live file carries its download URL.
	assert.JSONEq(t, `{"id":"0192c9b0-7e4a-7000-8000-000000000009","tenantId":"0192c9b0-7e4a-7000-8000-000000000002","userId":"0192c9b0-7e4a-7000-8000-000000000003","originalName":"a.pdf","mimeType":"application/pdf","size":42,"url":"/api/v1/files/0192c9b0-7e4a-7000-8000-000000000009/content","createdAt":"2026-01-02T03:04:05Z"}`, string(data))

	deleted := live
	deleted.DeletedAt = &created
	resp := toFileResponse(&deleted)
	assert.Empty(t, resp.URL, "a deleted file must not expose a download URL")
	assert.Equal(t, &created, resp.DeletedAt)
}
