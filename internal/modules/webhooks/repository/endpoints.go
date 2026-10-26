package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func toEndpoint(row webhooksqlc.WebhookEndpoint, eventTypes []string) webhooksvc.Endpoint {
	return webhooksvc.Endpoint{
		ID:          row.ID,
		TenantID:    row.TenantID,
		URL:         row.Url,
		Description: row.Description,
		EventTypes:  eventTypes,
		Status:      row.Status,
		MaxAttempts: row.MaxAttempts,
		TimeoutMS:   row.TimeoutMs,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
		DeletedAt:   timePtr(row.DeletedAt),
	}
}

// CreateEndpoint inserts the endpoint, its subscriptions, and its first
// secret in one transaction.
func (r *Postgres) CreateEndpoint(ctx context.Context, draft webhooksvc.CreateEndpointDraft) (*webhooksvc.Endpoint, error) {
	if r == nil || r.db == nil {
		return nil, ErrNoPool
	}
	tid := tenantID(ctx)
	var endpoint *webhooksvc.Endpoint
	err := r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		created, err := q.CreateWebhookEndpoint(ctx, webhooksqlc.CreateWebhookEndpointParams{
			TenantID:    tid,
			Url:         draft.URL,
			Description: draft.Description,
			Status:      draft.Status,
			MaxAttempts: draft.MaxAttempts,
			TimeoutMs:   draft.TimeoutMS,
		})
		if err != nil {
			return err
		}
		if err := q.CreateWebhookEndpointSubscriptions(ctx, webhooksqlc.CreateWebhookEndpointSubscriptionsParams{
			TenantID:   tid,
			EndpointID: created.ID,
			Patterns:   draft.EventTypes,
		}); err != nil {
			return err
		}
		if draft.SealSecret == nil {
			return fmt.Errorf("webhooks: create endpoint: secret sealer is required")
		}
		ciphertext, err := draft.SealSecret(created.ID)
		if err != nil {
			return err
		}
		if _, err := q.CreateWebhookEndpointSecret(ctx, webhooksqlc.CreateWebhookEndpointSecretParams{
			TenantID:         tid,
			EndpointID:       created.ID,
			Version:          1,
			SecretCiphertext: ciphertext,
		}); err != nil {
			return err
		}
		mapped := toEndpoint(created, draft.EventTypes)
		endpoint = &mapped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return endpoint, nil
}

func (r *Postgres) GetEndpoint(ctx context.Context, id int64) (*webhooksvc.Endpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookEndpoint(ctx, webhooksqlc.GetWebhookEndpointParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	endpoint := toEndpoint(row.WebhookEndpoint, row.EventTypes)
	return &endpoint, nil
}

func (r *Postgres) ListEndpoints(ctx context.Context, cur pagination.DBCursor) ([]webhooksvc.Endpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookEndpoints(ctx, webhooksqlc.ListWebhookEndpointsParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.Endpoint, len(rows))
	for i, row := range rows {
		out[i] = toEndpoint(row.WebhookEndpoint, row.EventTypes)
	}
	return out, nil
}

// UpdateEndpoint replaces the endpoint configuration and its subscription set
// in one transaction.
func (r *Postgres) UpdateEndpoint(ctx context.Context, draft webhooksvc.UpdateEndpointDraft) (*webhooksvc.Endpoint, error) {
	if r == nil || r.db == nil {
		return nil, ErrNoPool
	}
	tid := tenantID(ctx)
	var endpoint *webhooksvc.Endpoint
	err := r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		updated, err := q.UpdateWebhookEndpoint(ctx, webhooksqlc.UpdateWebhookEndpointParams{
			Url:         draft.URL,
			Description: draft.Description,
			Status:      draft.Status,
			MaxAttempts: draft.MaxAttempts,
			TimeoutMs:   draft.TimeoutMS,
			ID:          draft.ID,
			TenantID:    tid,
		})
		if err != nil {
			return mapNotFound(err)
		}
		// Queued deliveries snapshotted the old URL/timeout. Repoint them in
		// the same transaction so a retry after an endpoint move cannot keep
		// hitting the previous receiver.
		if err := q.UpdateWebhookDeliveryEndpointSnapshot(ctx, webhooksqlc.UpdateWebhookDeliveryEndpointSnapshotParams{
			Url:        draft.URL,
			TimeoutMs:  draft.TimeoutMS,
			TenantID:   tid,
			EndpointID: draft.ID,
		}); err != nil {
			return err
		}
		if err := q.DeleteWebhookEndpointSubscriptions(ctx, webhooksqlc.DeleteWebhookEndpointSubscriptionsParams{
			TenantID:   tid,
			EndpointID: draft.ID,
		}); err != nil {
			return err
		}
		if err := q.CreateWebhookEndpointSubscriptions(ctx, webhooksqlc.CreateWebhookEndpointSubscriptionsParams{
			TenantID:   tid,
			EndpointID: draft.ID,
			Patterns:   draft.EventTypes,
		}); err != nil {
			return err
		}
		mapped := toEndpoint(updated, draft.EventTypes)
		endpoint = &mapped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return endpoint, nil
}

// DeleteEndpoint soft-deletes and disables the endpoint. Its subscriptions
// stay: they only cascade on a hard delete.
func (r *Postgres) DeleteEndpoint(ctx context.Context, id int64) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	affected, err := r.q.DeleteWebhookEndpoint(ctx, webhooksqlc.DeleteWebhookEndpointParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperror.ErrNotFound
	}
	return nil
}

func (r *Postgres) RotateEndpointSecret(ctx context.Context, id int64, ciphertext string, expirePrevious bool) (*webhooksvc.Endpoint, error) {
	if r == nil || r.db == nil {
		return nil, ErrNoPool
	}
	tid := tenantID(ctx)
	var endpoint *webhooksvc.Endpoint
	err := r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		// Lock the endpoint row so concurrent rotations serialize instead of
		// both reading the same next version and failing the unique index.
		current, err := q.LockWebhookEndpoint(ctx, webhooksqlc.LockWebhookEndpointParams{ID: id, TenantID: tid})
		if err != nil {
			return mapNotFound(err)
		}
		version, err := q.NextWebhookEndpointSecretVersion(ctx, id)
		if err != nil {
			return err
		}
		if err := q.ExpireOlderWebhookEndpointSecrets(ctx, webhooksqlc.ExpireOlderWebhookEndpointSecretsParams{
			TenantID:   tid,
			EndpointID: id,
		}); err != nil {
			return err
		}
		graceSeconds := webhooksvc.SecretRotationGrace.Seconds()
		if expirePrevious {
			// A zero-length grace retires the replaced secret at commit time;
			// the worker will not co-sign with it on any later delivery.
			graceSeconds = 0
		}
		if err := q.DeactivateWebhookEndpointSecrets(ctx, webhooksqlc.DeactivateWebhookEndpointSecretsParams{
			TenantID:     tid,
			EndpointID:   id,
			GraceSeconds: graceSeconds,
		}); err != nil {
			return err
		}
		if expirePrevious {
			// Also close the grace of any older retired secret that was still
			// co-signing before this rotation.
			if err := q.ExpireOlderWebhookEndpointSecrets(ctx, webhooksqlc.ExpireOlderWebhookEndpointSecretsParams{
				TenantID:   tid,
				EndpointID: id,
			}); err != nil {
				return err
			}
		}
		if _, err := q.CreateWebhookEndpointSecret(ctx, webhooksqlc.CreateWebhookEndpointSecretParams{
			TenantID:         tid,
			EndpointID:       id,
			Version:          version,
			SecretCiphertext: ciphertext,
		}); err != nil {
			return err
		}
		mapped := toEndpoint(current.WebhookEndpoint, current.EventTypes)
		endpoint = &mapped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return endpoint, nil
}

func (r *Postgres) ActiveEndpoints(ctx context.Context, tenant int64) ([]webhooksvc.ActiveEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListActiveWebhookEndpointsWithSecret(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.ActiveEndpoint, len(rows))
	for i, row := range rows {
		out[i] = webhooksvc.ActiveEndpoint{
			ID:               row.ID,
			TenantID:         row.TenantID,
			URL:              row.Url,
			EventTypes:       row.EventTypes,
			MaxAttempts:      row.MaxAttempts,
			TimeoutMS:        row.TimeoutMs,
			SecretCiphertext: row.SecretCiphertext,
		}
	}
	return out, nil
}

func (r *Postgres) ActiveEndpointSecret(ctx context.Context, endpointID int64) (string, error) {
	if r == nil || r.q == nil {
		return "", ErrNoPool
	}
	secret, err := r.q.GetActiveWebhookEndpointSecret(ctx, webhooksqlc.GetActiveWebhookEndpointSecretParams{
		TenantID:   tenantID(ctx),
		EndpointID: endpointID,
	})
	if err != nil {
		return "", mapNotFound(err)
	}
	return secret.SecretCiphertext, nil
}

// SigningSecrets returns the endpoint's active sealed secret and, while its
// explicit grace window is open, its immediately previous sealed secret. The
// delivery worker co-signs with both so receivers can switch at any time
// during the window.
func (r *Postgres) SigningSecrets(ctx context.Context, endpointID int64) (*webhooksvc.SigningSecrets, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	secrets, err := r.q.GetWebhookEndpointSigningSecrets(ctx, webhooksqlc.GetWebhookEndpointSigningSecretsParams{
		TenantID:   tenantID(ctx),
		EndpointID: endpointID,
	})
	if err != nil {
		return nil, err
	}
	if secrets.CurrentCiphertext == "" {
		return nil, apperror.ErrNotFound
	}
	return &webhooksvc.SigningSecrets{
		CurrentCiphertext:  secrets.CurrentCiphertext,
		PreviousCiphertext: secrets.PreviousCiphertext,
	}, nil
}
