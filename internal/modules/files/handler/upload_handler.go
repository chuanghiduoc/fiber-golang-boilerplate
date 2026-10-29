package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// UploadHandler implements the files collection operations. The same
// operations serve a member's own files and, for a caller holding the
// admin:files:* permissions, every file in the request tenant; the service
// applies that narrowing.
type UploadHandler struct {
	service     filesvc.UploadService
	maxFileSize int64
	allowedMIME map[string]struct{}
}

func NewUploadHandler(svc filesvc.UploadService, maxFileSize int64, allowedTypes []string) *UploadHandler {
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}
	return &UploadHandler{service: svc, maxFileSize: maxFileSize, allowedMIME: allowed}
}

// CreateFile uploads multipart file data. The file_uploads flag is checked
// before the metadata transaction; a disabled flag returns 404
// feature.disabled and writes no file row.
func (h *UploadHandler) CreateFile(ctx context.Context, input *dto.CreateFileInput) (*httpapi.Created[dto.FileResponse], error) {
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}
	data := input.RawBody.Data()
	if data == nil || !data.File.IsSet || data.File.File == nil {
		return nil, apperror.NewBadRequest("file is required")
	}
	fileHeader := data.File
	stream := fileHeader.File
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			slog.Warn("failed to close uploaded file", slog.Any("error", closeErr))
		}
	}()

	if len([]rune(fileHeader.Filename)) > 255 {
		return nil, apperror.NewBadRequest("file name is too long")
	}
	if fileHeader.Size > h.maxFileSize {
		return nil, apperror.NewCode(filescatalog.CodeFileTooLarge,
			"file size exceeds the configured limit")
	}

	buf := make([]byte, 512)
	n, readErr := stream.Read(buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, apperror.Internal(readErr, "failed to read uploaded file")
	}
	contentType := http.DetectContentType(buf[:n])

	if len(h.allowedMIME) > 0 {
		if _, ok := h.allowedMIME[contentType]; !ok {
			return nil, apperror.NewCode(filescatalog.CodeFileUnsupportedType,
				"this file type is not allowed")
		}
	}

	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		return nil, apperror.Internal(err, "failed to process uploaded file")
	}

	result, err := h.service.Upload(ctx, userID, fileHeader.Filename, stream, fileHeader.Size, contentType)
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated("/api/v1/files/"+result.ID.String(), toFileResponse(result)), nil
}

// ListFiles lists the caller's files, or the whole tenant for an admin.
func (h *UploadHandler) ListFiles(ctx context.Context, input *dto.ListFilesInput) (*httpapi.ListOutput[dto.FileResponse], error) {
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}

	result, hasMore, err := h.service.List(ctx, userID, input.Limit, input.StartingAfter, input.IncludeDeleted)
	if err != nil {
		return nil, err
	}

	files := toFileResponses(result)
	next := ""
	if hasMore {
		next = pagination.NextCursor(files, func(f dto.FileResponse) (time.Time, uuid.UUID) {
			return f.CreatedAt, f.ID
		})
	}
	return &httpapi.ListOutput[dto.FileResponse]{
		Body: pagination.NewList("/api/v1/files", files, hasMore, next),
	}, nil
}

// GetFile returns one file's metadata.
func (h *UploadHandler) GetFile(ctx context.Context, input *dto.FileIDInput) (*httpapi.Output[dto.FileResponse], error) {
	id, err := parseFileID(input.FileID)
	if err != nil {
		return nil, err
	}
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}

	file, err := h.service.GetFileInfo(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.FileResponse]{Body: toFileResponse(file)}, nil
}

// GetFileContent streams the stored bytes with the stored media type and the
// original file name as an attachment.
func (h *UploadHandler) GetFileContent(ctx context.Context, input *dto.FileIDInput) (*huma.StreamResponse, error) {
	id, err := parseFileID(input.FileID)
	if err != nil {
		return nil, err
	}
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}

	file, reader, err := h.service.Download(ctx, id, userID)
	if err != nil {
		return nil, err
	}

	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	disposition := contentDispositionAttachment(file.OriginalName)
	size := file.Size
	fileID := file.ID
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer func() {
			if closeErr := reader.Close(); closeErr != nil {
				slog.Warn("failed to close downloaded file", slog.String("file_id", fileID.String()), slog.Any("error", closeErr))
			}
		}()
		hctx.SetHeader("Content-Type", mimeType)
		hctx.SetHeader("Content-Disposition", disposition)
		if size >= 0 {
			hctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
		}
		if _, copyErr := io.Copy(hctx.BodyWriter(), reader); copyErr != nil {
			slog.Error("failed to stream file content",
				slog.String("file_id", fileID.String()),
				slog.Any("error", copyErr),
			)
		}
	}}, nil
}

// DeleteFile soft-deletes a file so it can be restored until the trash purge.
func (h *UploadHandler) DeleteFile(ctx context.Context, input *dto.FileIDInput) (*httpapi.Empty, error) {
	id, err := parseFileID(input.FileID)
	if err != nil {
		return nil, err
	}
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}

	if err := h.service.Delete(ctx, id, userID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// RestoreFile revives a soft-deleted file.
func (h *UploadHandler) RestoreFile(ctx context.Context, input *dto.FileIDInput) (*httpapi.Output[dto.FileResponse], error) {
	id, err := parseFileID(input.FileID)
	if err != nil {
		return nil, err
	}
	userID, err := authUserID(ctx)
	if err != nil {
		return nil, err
	}

	file, err := h.service.Restore(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.FileResponse]{Body: toFileResponse(file)}, nil
}

// authUserID returns the authenticated principal. The route middleware has
// already authenticated the request; a missing subject is a programming error
// surfaced as 401 rather than an unauthenticated service call.
func authUserID(ctx context.Context) (uuid.UUID, error) {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok || subject.UserID == uuid.Nil {
		return uuid.Nil, apperror.NewUnauthorized("authentication is required")
	}
	return subject.UserID, nil
}

func parseFileID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, apperror.NewValidation("invalid fileId", []apperror.FieldError{{
			Path:    "path.fileId",
			Code:    "invalid",
			Message: "invalid fileId",
		}})
	}
	if id == uuid.Nil {
		return uuid.Nil, apperror.NewNotFound("resource not found")
	}
	return id, nil
}
