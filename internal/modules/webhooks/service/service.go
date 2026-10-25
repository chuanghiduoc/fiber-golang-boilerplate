package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// The service is split by responsibility within this package:
//
//	endpoints.go        outbound endpoint CRUD and validation
//	secrets.go          secret generation and rotation (endpoints, sources)
//	deliveries.go       delivery history, attempts, and replay
//	inbound_sources.go  inbound source CRUD
//	inbound_receive.go  the public inbound receiver and received events
//	retention.go        terminal-row retention

const (
	maxInboundPayloadBytes = 1 << 20
	defaultMaxAttempts     = 8
	defaultTimeoutMS       = 10_000
	maxEventIDLen          = 255
	secretPrefixEndpoint   = "whsec_"
	secretPrefixInbound    = "whin_"
	auditTimeout           = 2 * time.Second
)

// Options configures service defaults. Zero values fall back to safe defaults.
type Options struct {
	DefaultMaxAttempts int
	DefaultTimeoutMS   int32
	TimestampTolerance time.Duration
	// Tenants gates the public inbound receiver on the source tenant being
	// active. Nil skips the check (unit tests); production must set it.
	Tenants TenantChecker
}

// TenantChecker reports whether a tenant is active. The composition root
// adapts the identity tenant store, so this module never reads tenants.
type TenantChecker interface {
	TenantActive(ctx context.Context, tenantID int64) (bool, error)
}

// TenantCheckerFunc adapts a function to TenantChecker.
type TenantCheckerFunc func(ctx context.Context, tenantID int64) (bool, error)

// TenantActive implements TenantChecker.
func (f TenantCheckerFunc) TenantActive(ctx context.Context, tenantID int64) (bool, error) {
	return f(ctx, tenantID)
}

// Service is the webhooks application surface: tenant-scoped endpoint CRUD,
// secret rotation, delivery history/replay, inbound source CRUD, and the
// public inbound receiver.
type Service interface {
	CreateEndpoint(ctx context.Context, actorID int64, req dto.CreateEndpointRequest) (*dto.EndpointSecretResponse, error)
	UpdateEndpoint(ctx context.Context, actorID, id int64, req dto.UpdateEndpointRequest) (*dto.EndpointResponse, error)
	DeleteEndpoint(ctx context.Context, actorID, id int64) error
	GetEndpoint(ctx context.Context, id int64) (*dto.EndpointResponse, error)
	ListEndpoints(ctx context.Context, limit int, startingAfter string) ([]dto.EndpointResponse, bool, error)
	RotateEndpointSecret(ctx context.Context, actorID, id int64) (*dto.EndpointSecretResponse, error)

	ListDeliveries(ctx context.Context, endpointID int64, status string, limit int, startingAfter string) ([]dto.DeliveryResponse, bool, error)
	GetDelivery(ctx context.Context, id int64) (*dto.DeliveryResponse, error)
	ListDeliveryAttempts(ctx context.Context, id int64) ([]dto.DeliveryAttemptResponse, error)
	ReplayDelivery(ctx context.Context, actorID, id int64) (*dto.DeliveryResponse, error)

	CreateInboundSource(ctx context.Context, actorID int64, req dto.CreateInboundSourceRequest) (*dto.InboundSourceSecretResponse, error)
	UpdateInboundSource(ctx context.Context, actorID, id int64, req dto.UpdateInboundSourceRequest) (*dto.InboundSourceResponse, error)
	DeleteInboundSource(ctx context.Context, actorID, id int64) error
	GetInboundSource(ctx context.Context, id int64) (*dto.InboundSourceResponse, error)
	ListInboundSources(ctx context.Context, limit int, startingAfter string) ([]dto.InboundSourceResponse, bool, error)
	RotateInboundSourceSecret(ctx context.Context, actorID, id int64) (*dto.InboundSourceSecretResponse, error)
	ListInboundEvents(ctx context.Context, sourceID int64, limit int, startingAfter string) ([]dto.InboundEventResponse, bool, error)
	ReceiveInbound(ctx context.Context, sourceID int64, headers http.Header, body []byte) (*dto.InboundReceipt, error)

	PurgeRetention(ctx context.Context, retentionDays int32) (RetentionResult, error)
}

type service struct {
	endpoints  repository.EndpointStore
	deliveries repository.DeliveryStore
	inbound    repository.InboundStore
	codec      *SecretCodec
	audit      audit.Recorder
	opts       Options
	now        func() time.Time
	urlGuard   *webhook.Guard
}

// New returns the webhooks service. audit may be nil (Noop semantics).
func New(
	endpoints repository.EndpointStore,
	deliveries repository.DeliveryStore,
	inbound repository.InboundStore,
	codec *SecretCodec,
	recorder audit.Recorder,
	opts Options,
) Service {
	if opts.DefaultMaxAttempts <= 0 {
		opts.DefaultMaxAttempts = defaultMaxAttempts
	}
	if opts.DefaultTimeoutMS <= 0 {
		opts.DefaultTimeoutMS = defaultTimeoutMS
	}
	if opts.TimestampTolerance <= 0 {
		opts.TimestampTolerance = webhook.DefaultTolerance
	}
	guard, _ := webhook.NewGuard(nil)
	return &service{
		endpoints:  endpoints,
		deliveries: deliveries,
		inbound:    inbound,
		codec:      codec,
		audit:      recorder,
		opts:       opts,
		now:        func() time.Time { return time.Now().UTC() },
		urlGuard:   guard,
	}
}

func (s *service) recordAudit(ctx context.Context, actorID int64, action, resource string, resourceID int64, metadata map[string]any) {
	if s.audit == nil {
		return
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	event := audit.Event{
		TenantID:   tenant.EffectiveID(ctx),
		ActorID:    actorID,
		Action:     action,
		Resource:   resource,
		ResourceID: fmt.Sprintf("%d", resourceID),
		Metadata:   metadata,
		CreatedAt:  s.now(),
	}
	// Link the domain event to the route's audithttp.Guard intent row, if any.
	// The tenant stays the service's effective tenant: the public receiver
	// resolves it from the source, not from the request.
	if info := audit.RequestInfoFromContext(ctx); info != nil {
		event.ParentID = info.EventID
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := s.audit.Record(auditCtx, event); err != nil {
		slog.Warn("webhooks: failed to record audit event",
			slog.String("action", action),
			slog.Any("error", err),
		)
	}
}

func validStatus(status string) bool {
	return status == dto.StatusActive || status == dto.StatusDisabled
}

func buildCursor(limit int, startingAfter string) (pagination.DBCursor, int, error) {
	pageSize := pagination.NormalizeLimit(limit)
	cur := pagination.DBCursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, err := pagination.DecodeCursor(startingAfter)
		if err != nil {
			return pagination.DBCursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}

func isConflict(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == http.StatusConflict
}
