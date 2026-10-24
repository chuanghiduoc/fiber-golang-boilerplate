package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func (r *Postgres) CreateEndpoint(ctx context.Context, draft CreateEndpointDraft) (*webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNoPool
	}
	tid := tenantID(ctx)
	var endpoint webhooksqlc.WebhookEndpoint
	err := r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		created, err := q.CreateWebhookEndpoint(ctx, webhooksqlc.CreateWebhookEndpointParams{
			TenantID:    tid,
			Url:         draft.URL,
			Description: draft.Description,
			EventTypes:  draft.EventTypes,
			Status:      draft.Status,
			MaxAttempts: draft.MaxAttempts,
			TimeoutMs:   draft.TimeoutMS,
		})
		if err != nil {
			return err
		}
		if _, err := q.CreateWebhookEndpointSecret(ctx, webhooksqlc.CreateWebhookEndpointSecretParams{
			TenantID:         tid,
			EndpointID:       created.ID,
			Version:          1,
			SecretCiphertext: draft.SecretCiphertext,
		}); err != nil {
			return err
		}
		endpoint = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &endpoint, nil
}

func (r *Postgres) GetEndpoint(ctx context.Context, id int64) (*webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	endpoint, err := r.q.GetWebhookEndpoint(ctx, webhooksqlc.GetWebhookEndpointParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &endpoint, nil
}

func (r *Postgres) ListEndpoints(ctx context.Context, cur pagination.DBCursor) ([]webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	return r.q.ListWebhookEndpoints(ctx, webhooksqlc.ListWebhookEndpointsParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
}

func (r *Postgres) UpdateEndpoint(ctx context.Context, draft UpdateEndpointDraft) (*webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	endpoint, err := r.q.UpdateWebhookEndpoint(ctx, webhooksqlc.UpdateWebhookEndpointParams{
		Url:         draft.URL,
		Description: draft.Description,
		EventTypes:  draft.EventTypes,
		Status:      draft.Status,
		MaxAttempts: draft.MaxAttempts,
		TimeoutMs:   draft.TimeoutMS,
		ID:          draft.ID,
		TenantID:    tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &endpoint, nil
}

func (r *Postgres) DeleteEndpoint(ctx context.Context, id int64) (*webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	endpoint, err := r.q.DeleteWebhookEndpoint(ctx, webhooksqlc.DeleteWebhookEndpointParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &endpoint, nil
}

func (r *Postgres) RotateEndpointSecret(ctx context.Context, id int64, ciphertext string) (*webhooksqlc.WebhookEndpoint, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNoPool
	}
	tid := tenantID(ctx)
	var endpoint webhooksqlc.WebhookEndpoint
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
		if err := q.DeactivateWebhookEndpointSecrets(ctx, webhooksqlc.DeactivateWebhookEndpointSecretsParams{
			TenantID:   tid,
			EndpointID: id,
		}); err != nil {
			return err
		}
		if _, err := q.CreateWebhookEndpointSecret(ctx, webhooksqlc.CreateWebhookEndpointSecretParams{
			TenantID:         tid,
			EndpointID:       id,
			Version:          version,
			SecretCiphertext: ciphertext,
		}); err != nil {
			return err
		}
		endpoint = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &endpoint, nil
}

func (r *Postgres) ActiveEndpoints(ctx context.Context, tenant int64) ([]ActiveEndpoint, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListActiveWebhookEndpointsWithSecret(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]ActiveEndpoint, len(rows))
	for i, row := range rows {
		out[i] = ActiveEndpoint{
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
