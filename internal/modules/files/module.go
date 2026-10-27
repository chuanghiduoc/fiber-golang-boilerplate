package files

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module owns file upload, metadata, download, soft delete, and restore.
type Module struct {
	upload          *fileshandler.UploadHandler
	trashPurger     *filesvc.TrashPurger
	orphanSweeper   *filesvc.OrphanSweeper
	privacyExporter *filesvc.PrivacyExporter
	privacyEraser   *filesvc.PrivacyEraser
	tenantPurger    tenancycontracts.Purger
	provides        Provides
}

var (
	_ app.RouteProvider    = (*Module)(nil)
	_ app.JobProvider      = (*Module)(nil)
	_ app.ScheduleProvider = (*Module)(nil)
	_ app.PrivacyProvider  = (*Module)(nil)
)

// New builds the module from its handler. Production uses Build.
func New(upload *fileshandler.UploadHandler) *Module {
	return &Module{upload: upload}
}

func (m *Module) Name() string { return filescatalog.Owner }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacyExporter == nil || m.privacyEraser == nil {
		return
	}
	registry.Register(privacycontracts.Provider{
		Name: "files", Exporter: m.privacyExporter, Eraser: m.privacyEraser,
	})
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return filescatalog.Permissions() }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) Errors() []apperror.Spec { return filescatalog.Errors() }

// FeatureFlags implements app.FlagProvider.
func (m *Module) FeatureFlags() []features.Definition { return filescatalog.FeatureFlags() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return filescatalog.Locales() }

// RegisterRoutes implements app.RouteProvider with the canonical /files
// collection. The route policy is the same for a member and a tenant admin;
// the service narrows a caller without admin:files:* to their own rows and
// conceals cross-tenant ids as 404.
func (m *Module) RegisterRoutes(api *httpapi.API, deps app.Deps) {
	const collectionPath = "/api/v1/files"

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        collectionPath,
		OperationID: "listFiles",
		Tags:        []string{"files"},
		Summary:     "List files",
		Description: "Lists the caller's files. A caller holding `admin:files:read` sees every file in the tenant.",
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesRead,
		RateLimit:  httpapi.RateLimitRelaxed,
	}, m.upload.ListFiles)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        collectionPath,
		OperationID: "createFile",
		Tags:        []string{"files"},
		Summary:     "Upload a file",
		Description: "Stores a multipart file and its metadata. The `file_uploads` feature flag gates the operation before idempotency and audit.",
		Errors:      []int{http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusServiceUnavailable},
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesWrite,
		RateLimit:  httpapi.RateLimitNormal,
		Feature:    filescatalog.KeyFileUploads,
		Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
			Action: "file.uploaded", Resource: "file",
		}},
	}, func(ctx context.Context, input *dto.CreateFileInput) (*httpapi.Created[dto.FileResponse], error) {
		return m.upload.CreateFile(ctx, input)
	})

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        collectionPath + "/{fileId}",
		OperationID: "getFile",
		Tags:        []string{"files"},
		Summary:     "Get file metadata",
		Description: "Returns one file's metadata. Another owner's file is visible only with `admin:files:read`; a cross-tenant id is 404.",
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesRead,
		RateLimit:  httpapi.RateLimitRelaxed,
	}, m.upload.GetFile)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        collectionPath + "/{fileId}/content",
		OperationID: "getFileContent",
		Tags:        []string{"files"},
		Summary:     "Download file content",
		Description: "Streams the stored bytes as an attachment. Responses are never cached.",
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesRead,
		RateLimit:  httpapi.RateLimitRelaxed,
		NoStore:    true,
	}, m.upload.GetFileContent)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodDelete,
		Path:        collectionPath + "/{fileId}",
		OperationID: "deleteFile",
		Tags:        []string{"files"},
		Summary:     "Delete a file",
		Description: "Soft-deletes a file so it stays restorable until the trash purge. Another owner's file needs `admin:files:delete`.",
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesDelete,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
			Action: "file.deleted", Resource: "file",
		}},
	}, m.upload.DeleteFile)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        collectionPath + "/{fileId}/restore",
		OperationID: "restoreFile",
		Tags:        []string{"files"},
		Summary:     "Restore a file",
		Description: "Revives a soft-deleted file. Another owner's file needs `admin:files:restore`.",
	}, httpapi.Policy{
		Permission: filescatalog.PermissionFilesRestore,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
			Action: "file.restored", Resource: "file",
		}},
	}, m.upload.RestoreFile)

	documentFileContent(api)
}

// documentFileContent adds the binary response schema and
// Content-Disposition header to getFileContent. StreamResponse writes the
// bytes directly, so huma documents an empty 200 unless the module supplies
// the schema for the streamed body.
func documentFileContent(api *httpapi.API) {
	path := api.Huma().OpenAPI().Paths["/api/v1/files/{fileId}/content"]
	if path == nil || path.Get == nil {
		panic("files: getFileContent operation is missing from the OpenAPI document")
	}
	response := path.Get.Responses["200"]
	if response == nil {
		panic("files: getFileContent has no documented 200 response")
	}
	response.Content = map[string]*huma.MediaType{
		"application/octet-stream": {
			Schema: &huma.Schema{
				Type:        "string",
				Format:      "binary",
				Description: "Stored file bytes; Content-Type carries the file's media type.",
			},
		},
	}
	if response.Headers == nil {
		response.Headers = map[string]*huma.Param{}
	}
	response.Headers["Content-Disposition"] = &huma.Param{
		Description: "Attachment disposition with the original file name.",
		Schema:      &huma.Schema{Type: "string"},
	}
}

// Admin returns the files half of the admin surface.
func (m *Module) Admin() filescontracts.FilesAdmin { return m.provides.Admin }
