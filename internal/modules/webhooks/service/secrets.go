package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// Labels used in the public error detail when a secret cannot be produced.
const (
	endpointSecretLabel = "webhook secret"
	inboundSecretLabel  = "inbound secret"
)

// newSecret generates a Standard Webhooks "whsec_<base64>" secret (for
// endpoints and inbound sources alike, so the official libraries accept it
// unchanged). The plaintext is returned to the caller exactly once and is
// never persisted; label names the secret in public errors.
func newSecret(label string) (string, error) {
	secret, err := webhook.NewSecret()
	if err != nil {
		return "", apperror.NewInternal("failed to generate " + label)
	}
	return secret, nil
}

// sealSecret seals a generated secret for its purpose, tenant, and owning row.
func (s *service) sealSecret(label string, binding SecretBinding, secret string) (string, error) {
	ciphertext, err := s.codec.Seal(binding, []byte(secret))
	if err != nil {
		return "", apperror.NewInternal("failed to protect " + label)
	}
	return ciphertext, nil
}

// newSealedSecret generates and seals a secret whose owning row already exists
// (rotation paths).
func (s *service) newSealedSecret(binding SecretBinding, label string) (secret, ciphertext string, err error) {
	secret, err = newSecret(label)
	if err != nil {
		return "", "", err
	}
	ciphertext, err = s.sealSecret(label, binding, secret)
	if err != nil {
		return "", "", err
	}
	return secret, ciphertext, nil
}

// RotateEndpointSecret makes a fresh secret active for new deliveries. The
// delivery worker keeps dual-signing pending retries during the rotation grace.
func (s *service) RotateEndpointSecret(ctx context.Context, actorID, id int64) (*Endpoint, string, error) {
	tenantID := tenant.EffectiveID(ctx)
	secret, ciphertext, err := s.newSealedSecret(EndpointSecretBinding(tenantID, id), endpointSecretLabel)
	if err != nil {
		return nil, "", err
	}
	endpoint, err := s.endpoints.RotateEndpointSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, "", apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, "", apperror.NewInternal("failed to rotate webhook secret")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.secret_rotated", "webhook_endpoint", id, nil)
	return endpoint, secret, nil
}

// RotateInboundSourceSecret replaces the secret the sender signs with.
func (s *service) RotateInboundSourceSecret(ctx context.Context, actorID, id int64) (*InboundSource, string, error) {
	tenantID := tenant.EffectiveID(ctx)
	secret, ciphertext, err := s.newSealedSecret(InboundSourceSecretBinding(tenantID, id), inboundSecretLabel)
	if err != nil {
		return nil, "", err
	}
	source, err := s.inbound.RotateInboundSourceSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, "", apperror.NewNotFound("inbound source not found")
		}
		return nil, "", apperror.NewInternal("failed to rotate inbound secret")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.secret_rotated", "webhook_inbound_source", id, nil)
	return source, secret, nil
}
