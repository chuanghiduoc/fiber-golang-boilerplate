package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// Labels used in the public error detail when a secret cannot be produced.
const (
	endpointSecretLabel = "webhook secret"
	inboundSecretLabel  = "inbound secret"
)

// newSealedSecret generates a Standard Webhooks "whsec_<base64>" secret (for
// endpoints and inbound sources alike, so the official libraries accept it
// unchanged) and seals it for storage. The plaintext is returned to the
// caller exactly once (create or rotate) and is never persisted; label names
// the secret in public errors.
func (s *service) newSealedSecret(label string) (secret, ciphertext string, err error) {
	secret, err = webhook.NewSecret()
	if err != nil {
		return "", "", apperror.NewInternal("failed to generate " + label)
	}
	ciphertext, err = s.codec.Seal([]byte(secret))
	if err != nil {
		return "", "", apperror.NewInternal("failed to protect " + label)
	}
	return secret, ciphertext, nil
}

// RotateEndpointSecret makes a fresh secret active for new deliveries. The
// delivery worker keeps dual-signing pending retries during the rotation grace.
func (s *service) RotateEndpointSecret(ctx context.Context, actorID, id int64) (*dto.EndpointSecretResponse, error) {
	secret, ciphertext, err := s.newSealedSecret(endpointSecretLabel)
	if err != nil {
		return nil, err
	}
	endpoint, err := s.endpoints.RotateEndpointSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to rotate webhook secret")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.secret_rotated", "webhook_endpoint", id, nil)
	resp := toEndpointResponse(endpoint)
	return &dto.EndpointSecretResponse{EndpointResponse: resp, Secret: secret}, nil
}

// RotateInboundSourceSecret replaces the secret the sender signs with.
func (s *service) RotateInboundSourceSecret(ctx context.Context, actorID, id int64) (*dto.InboundSourceSecretResponse, error) {
	secret, ciphertext, err := s.newSealedSecret(inboundSecretLabel)
	if err != nil {
		return nil, err
	}
	source, err := s.inbound.RotateInboundSourceSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to rotate inbound secret")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.secret_rotated", "webhook_inbound_source", id, nil)
	resp := toInboundSourceResponse(source)
	return &dto.InboundSourceSecretResponse{InboundSourceResponse: resp, Secret: secret}, nil
}
