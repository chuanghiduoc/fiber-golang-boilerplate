// Package handler exposes the API-key lifecycle through huma operations. It
// only translates HTTP inputs and outputs; ownership, rank, scope, and audit
// rules live in the service.
package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const apiKeysPath = httpapi.APIPrefix + "/me/api-keys/"

// Handler serves the API-key operations.
type Handler struct {
	service apikeysvc.Service
}

func NewHandler(service apikeysvc.Service) *Handler {
	return &Handler{service: service}
}

// ListMineInput is the query for the caller's page of keys.
type ListMineInput struct {
	Limit         int    `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Maximum number of keys to return."`
	StartingAfter string `query:"startingAfter" doc:"Opaque cursor from the previous page."`
}

// CreateInput is the create request body.
type CreateInput struct {
	Body apikeydto.CreateAPIKeyRequest
}

// ListMemberInput is the query for one member's page of keys.
type ListMemberInput struct {
	MemberID      uuid.UUID `path:"memberId" doc:"Member id (UUID) whose keys are listed."`
	Limit         int       `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Maximum number of keys to return."`
	StartingAfter string    `query:"startingAfter" doc:"Opaque cursor from the previous page."`
}

// KeyInput addresses the caller's key.
type KeyInput struct {
	KeyID uuid.UUID `path:"keyId" doc:"API key id (UUID)."`
}

// UpdateMineInput is a merge-patch request for the caller's key.
type UpdateMineInput struct {
	KeyID uuid.UUID                     `path:"keyId" doc:"API key id (UUID)."`
	Body  apikeydto.UpdateAPIKeyRequest `contentType:"application/merge-patch+json"`
}

// MemberKeyInput addresses a key owned by a member.
type MemberKeyInput struct {
	MemberID uuid.UUID `path:"memberId" doc:"Member id (UUID) whose key is addressed."`
	KeyID    uuid.UUID `path:"keyId" doc:"API key id (UUID)."`
}

// UpdateMemberInput is a merge-patch request for another member's key.
type UpdateMemberInput struct {
	MemberID uuid.UUID                     `path:"memberId" doc:"Member id (UUID) whose key is addressed."`
	KeyID    uuid.UUID                     `path:"keyId" doc:"API key id (UUID)."`
	Body     apikeydto.UpdateAPIKeyRequest `contentType:"application/merge-patch+json"`
}

// ListMine returns the caller's own API keys.
func (h *Handler) ListMine(ctx context.Context, in *ListMineInput) (*httpapi.ListOutput[apikeydto.APIKeyResponse], error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	keys, hasMore, err := h.service.ListOwnedBy(ctx, caller, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	return listOutput(ctx, keys, hasMore), nil
}

// CreateMine creates a key owned by the authenticated caller. The plaintext
// key is returned exactly once.
func (h *Handler) CreateMine(ctx context.Context, in *CreateInput) (*httpapi.Created[apikeydto.CreatedAPIKeyResponse], error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	key, err := h.service.Create(ctx, caller, apikeysvc.CreateInput{
		Name:      in.Body.Name,
		Scopes:    in.Body.Scopes,
		ExpiresAt: in.Body.ExpiresAt,
	})
	if err != nil {
		return nil, err
	}
	resp := toCreatedAPIKeyResponse(key)
	return httpapi.NewCreated(apiKeysPath+resp.ID.String(), resp), nil
}

// GetMine returns one of the caller's keys.
func (h *Handler) GetMine(ctx context.Context, in *KeyInput) (*httpapi.Output[apikeydto.APIKeyResponse], error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	key, err := h.service.GetOwnedBy(ctx, caller, in.KeyID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[apikeydto.APIKeyResponse]{Body: toAPIKeyResponse(key)}, nil
}

// UpdateMine applies an RFC 7396 merge patch to the caller's key.
func (h *Handler) UpdateMine(ctx context.Context, in *UpdateMineInput) (*httpapi.Output[apikeydto.APIKeyResponse], error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	update, err := mergeUpdate(in.Body)
	if err != nil {
		return nil, err
	}
	key, err := h.service.UpdateOwnedBy(ctx, caller, in.KeyID, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[apikeydto.APIKeyResponse]{Body: toAPIKeyResponse(key)}, nil
}

// DeleteMine revokes the caller's key.
func (h *Handler) DeleteMine(ctx context.Context, in *KeyInput) (*httpapi.Empty, error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	if err := h.service.RevokeOwnedBy(ctx, caller, in.KeyID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// RotateMine issues a replacement key for the caller's key and returns the new
// plaintext credential exactly once.
func (h *Handler) RotateMine(ctx context.Context, in *KeyInput) (*httpapi.Created[apikeydto.RotatedAPIKeyResponse], error) {
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	rotated, err := h.service.Rotate(ctx, caller, in.KeyID)
	if err != nil {
		return nil, err
	}
	resp := toRotatedAPIKeyResponse(rotated)
	return httpapi.NewCreated(apiKeysPath+resp.ID.String(), resp), nil
}

// ListMember returns another member's keys. The route permission is
// admin:api_keys:read; the page is scoped to the member id on the path.
func (h *Handler) ListMember(ctx context.Context, in *ListMemberInput) (*httpapi.ListOutput[apikeydto.APIKeyResponse], error) {
	if err := requireID(in.MemberID, "memberId"); err != nil {
		return nil, err
	}
	keys, hasMore, err := h.service.ListForMember(ctx, in.MemberID, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	return listOutput(ctx, keys, hasMore), nil
}

// UpdateMember applies an RFC 7396 merge patch to another member's key.
func (h *Handler) UpdateMember(ctx context.Context, in *UpdateMemberInput) (*httpapi.Output[apikeydto.APIKeyResponse], error) {
	if err := requireID(in.MemberID, "memberId"); err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	update, err := mergeUpdate(in.Body)
	if err != nil {
		return nil, err
	}
	key, err := h.service.UpdateForMember(ctx, in.MemberID, in.KeyID, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[apikeydto.APIKeyResponse]{Body: toAPIKeyResponse(key)}, nil
}

// DeleteMember revokes another member's key.
func (h *Handler) DeleteMember(ctx context.Context, in *MemberKeyInput) (*httpapi.Empty, error) {
	if err := requireID(in.MemberID, "memberId"); err != nil {
		return nil, err
	}
	if err := requireID(in.KeyID, "keyId"); err != nil {
		return nil, err
	}
	if err := h.service.RevokeForMember(ctx, in.MemberID, in.KeyID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

func listOutput(ctx context.Context, keys []apikeysvc.APIKey, hasMore bool) *httpapi.ListOutput[apikeydto.APIKeyResponse] {
	responses := toAPIKeyResponses(keys)
	cursor := pagination.NextCursor(responses, func(k apikeydto.APIKeyResponse) (time.Time, uuid.UUID) {
		return k.CreatedAt, k.ID
	})
	return &httpapi.ListOutput[apikeydto.APIKeyResponse]{
		Body: pagination.NewList(requestctx.Path(ctx), responses, hasMore, cursor),
	}
}

// mergeUpdate turns a merge-patch body into the service input. An absent
// member is unchanged; an explicit null is refused because both mutable
// fields are required when present.
func mergeUpdate(body apikeydto.UpdateAPIKeyRequest) (apikeysvc.UpdateInput, error) {
	var update apikeysvc.UpdateInput
	if body.Name.Present {
		if body.Name.Null {
			return update, apperror.NewBadRequest("name cannot be null")
		}
		name := body.Name.Value
		update.Name = &name
	}
	if body.Scopes.Present {
		if body.Scopes.Null {
			return update, apperror.NewBadRequest("scopes cannot be null")
		}
		update.Scopes = body.Scopes.Value
	}
	if update.Name == nil && update.Scopes == nil {
		return update, apperror.NewBadRequest("at least one of name or scopes is required")
	}
	return update, nil
}

func callerID(ctx context.Context) (uuid.UUID, error) {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok || subject.UserID == uuid.Nil {
		return uuid.Nil, apperror.NewUnauthorized("authenticated user is required")
	}
	return subject.UserID, nil
}

// requireID rejects the nil UUID, which is syntactically valid in a path but
// can never identify a stored resource.
func requireID(id uuid.UUID, name string) error {
	if id == uuid.Nil {
		return apperror.NewBadRequest("invalid " + name)
	}
	return nil
}

// toAPIKeyResponse maps a service key to the wire contract. The hash and
// plaintext never appear.
func toAPIKeyResponse(key *apikeysvc.APIKey) apikeydto.APIKeyResponse {
	scopes := make([]string, len(key.Scopes))
	for i, scope := range key.Scopes {
		scopes[i] = string(scope)
	}
	return apikeydto.APIKeyResponse{
		ID:                 key.ID,
		UserID:             key.UserID,
		Name:               key.Name,
		Prefix:             key.Prefix,
		Scopes:             scopes,
		ExpiresAt:          key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}
}

func toAPIKeyResponses(keys []apikeysvc.APIKey) []apikeydto.APIKeyResponse {
	out := make([]apikeydto.APIKeyResponse, len(keys))
	for i := range keys {
		out[i] = toAPIKeyResponse(&keys[i])
	}
	return out
}

func toCreatedAPIKeyResponse(key *apikeysvc.CreatedKey) apikeydto.CreatedAPIKeyResponse {
	return apikeydto.CreatedAPIKeyResponse{
		APIKeyResponse: toAPIKeyResponse(&key.APIKey),
		Key:            key.Secret,
	}
}

func toRotatedAPIKeyResponse(key *apikeysvc.RotatedKey) apikeydto.RotatedAPIKeyResponse {
	return apikeydto.RotatedAPIKeyResponse{
		CreatedAPIKeyResponse: toCreatedAPIKeyResponse(&key.CreatedKey),
		PreviousKeyID:         key.PreviousKeyID,
		PreviousKeyValidUntil: key.PreviousKeyValidUntil,
	}
}
