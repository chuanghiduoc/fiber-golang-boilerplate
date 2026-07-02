package handler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

type UploadHandler struct {
	service     service.UploadService
	maxFileSize int64
	allowedMIME map[string]struct{}
}

func NewUploadHandler(svc service.UploadService, maxFileSize int64, allowedTypes []string) *UploadHandler {
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}
	return &UploadHandler{service: svc, maxFileSize: maxFileSize, allowedMIME: allowed}
}

// Upload godoc
// @Summary Upload a file
// @Description Upload a file to storage
// @Tags Files
// @Accept multipart/form-data
// @Produce json
// @Security BearerAuth
// @Param file formData file true "File to upload"
// @Success 201 {object} dto.FileResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /files/upload [post]
func (h *UploadHandler) Upload(c fiber.Ctx) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return apperror.NewBadRequest("file is required")
	}

	if fileHeader.Size > h.maxFileSize {
		return apperror.NewBadRequest(fmt.Sprintf("file size exceeds %dMB limit", h.maxFileSize/(1<<20)))
	}

	file, err := fileHeader.Open()
	if err != nil {
		return apperror.NewInternal("failed to open uploaded file")
	}
	defer func() { _ = file.Close() }()

	// Detect actual MIME type from file content
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return apperror.NewInternal("failed to read uploaded file")
	}
	contentType := http.DetectContentType(buf[:n])

	if len(h.allowedMIME) > 0 {
		if _, ok := h.allowedMIME[contentType]; !ok {
			return apperror.NewBadRequest(fmt.Sprintf("file type %q is not allowed", contentType))
		}
	}

	// Seek back to start so the service reads the full file
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return apperror.NewInternal("failed to process uploaded file")
	}

	result, err := h.service.Upload(c.Context(), authUserID(c), fileHeader.Filename, file, fileHeader.Size, contentType)
	if err != nil {
		return err
	}

	return response.Created(c, result)
}

// GetInfo godoc
// @Summary Get file info
// @Description Get file metadata by ID
// @Tags Files
// @Produce json
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 200 {object} dto.FileResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /files/{id} [get]
func (h *UploadHandler) GetInfo(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	file, err := h.service.GetFileInfo(c.Context(), id, authUserID(c))
	if err != nil {
		return err
	}

	return response.Success(c, file)
}

// Download godoc
// @Summary Download a file
// @Description Download a file by ID
// @Tags Files
// @Produce octet-stream
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 200
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /files/{id}/download [get]
func (h *UploadHandler) Download(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	userID := authUserID(c)

	file, reader, err := h.service.Download(c.Context(), id, userID)
	if err != nil {
		return err
	}
	// Note: do NOT defer reader.Close() here.
	// SendStream sets the reader as the response body stream; fasthttp reads
	// it after the handler returns and closes it automatically (io.Closer).

	c.Set("Content-Type", file.MimeType)
	c.Set("Content-Disposition", contentDispositionAttachment(file.OriginalName))
	c.Set("Content-Length", strconv.FormatInt(file.Size, 10))

	return c.SendStream(reader)
}

// List godoc
// @Summary List user's files
// @Description Get a paginated list of the authenticated user's files
// @Tags Files
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.FileResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Router /files [get]
func (h *UploadHandler) List(c fiber.Ctx) error {
	limit, startingAfter, err := cursorQuery(c)
	if err != nil {
		return err
	}

	files, hasMore, err := h.service.List(c.Context(), authUserID(c), limit, startingAfter)
	if err != nil {
		return err
	}

	return response.List(c, files, hasMore)
}

// Delete godoc
// @Summary Delete a file
// @Description Delete a file by ID (ownership check)
// @Tags Files
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /files/{id} [delete]
func (h *UploadHandler) Delete(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	if err := h.service.Delete(c.Context(), id, authUserID(c)); err != nil {
		return err
	}

	return response.NoContent(c)
}
