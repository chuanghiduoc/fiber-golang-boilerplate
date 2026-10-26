package handler

import (
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
)

func toCreateInput(req apikeydto.CreateRequest) apikeysvc.CreateInput {
	return apikeysvc.CreateInput{
		Name:      req.Name,
		Scopes:    req.Scopes,
		ExpiresAt: req.ExpiresAt,
	}
}

func toUpdateInput(req apikeydto.UpdateRequest) apikeysvc.UpdateInput {
	return apikeysvc.UpdateInput{Name: req.Name, Scopes: req.Scopes}
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

func toCreatedKeyResponse(key *apikeysvc.CreatedKey) apikeydto.KeyWithSecretResponse {
	return apikeydto.KeyWithSecretResponse{
		APIKeyResponse: toAPIKeyResponse(&key.APIKey),
		Key:            key.Secret,
	}
}

func toRotatedKeyResponse(key *apikeysvc.RotatedKey) apikeydto.RotateResponse {
	return apikeydto.RotateResponse{
		KeyWithSecretResponse: toCreatedKeyResponse(&key.CreatedKey),
		PreviousKeyID:         key.PreviousKeyID,
		PreviousKeyValidUntil: key.PreviousKeyValidUntil,
	}
}
