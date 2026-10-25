package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	adminsvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
)

func TestToFileResponse(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	live := filescontracts.File{ID: 9, UserID: 3, OriginalName: "a.pdf", MimeType: "application/pdf", Size: 42, CreatedAt: created}

	data, err := json.Marshal(toFileResponse(live))
	require.NoError(t, err)
	// A live file carries its admin download URL.
	assert.JSONEq(t, `{"id":9,"userId":3,"originalName":"a.pdf","mimeType":"application/pdf","size":42,"url":"/api/v1/admin/files/9/download","createdAt":"2026-01-02T03:04:05Z"}`, string(data))

	deleted := live
	deleted.DeletedAt = &created
	resp := toFileResponse(deleted)
	assert.Empty(t, resp.URL, "a deleted file must not expose a download URL")
	assert.Equal(t, &created, resp.DeletedAt)
}

func TestToStatsResponse(t *testing.T) {
	stats := adminsvc.Stats{ActiveUsers: 10, DeletedUsers: 2, TotalFiles: 5, TotalFileSize: 50000}

	resp := toStatsResponse(stats)

	assert.Equal(t, int64(10), resp.ActiveUsers)
	assert.Equal(t, int64(2), resp.DeletedUsers)
	assert.Equal(t, int64(5), resp.TotalFiles)
	assert.Equal(t, int64(50000), resp.TotalFileSize)
}
