// Package apikeydto contains the API-key wire contract, declared as huma
// inputs and outputs. Plaintext credentials appear only in create/rotate
// responses.
package apikeydto

import (
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

// CreateAPIKeyRequest asks for a new key owned by the authenticated caller.
// Scopes must already be held by the caller through its role or key. An
// omitted expiresAt defaults to APIKEYS_MAX_LIFETIME from now.
type CreateAPIKeyRequest struct {
	Name      string     `json:"name" required:"true" minLength:"1" maxLength:"255" doc:"Human-readable key name."`
	Scopes    []string   `json:"scopes" required:"true" minItems:"1" maxItems:"100" doc:"Permissions the key may exercise; must be held by the caller."`
	ExpiresAt *time.Time `json:"expiresAt,omitempty" doc:"Absolute expiry; defaults to the configured maximum lifetime."`
}

// UpdateAPIKeyRequest is an RFC 7396 merge patch. An absent member leaves the
// current value unchanged; an explicit null is invalid because name and scopes
// are required.
type UpdateAPIKeyRequest struct {
	Name   OptionalString  `json:"name,omitempty" doc:"Replacement human-readable name."`
	Scopes OptionalStrings `json:"scopes,omitempty" doc:"Replacement scope set; must remain within the owner's grants."`
}

// OptionalString is pkg/patch.Field[string] with the value's OpenAPI schema so
// huma validates the scalar body member instead of the field wrapper's struct.
type OptionalString struct {
	patch.Field[string]
}

// UnmarshalJSON delegates presence/null tracking to patch.Field.
func (o *OptionalString) UnmarshalJSON(data []byte) error { return o.Field.UnmarshalJSON(data) }

// Schema declares the member as a nullable string.
func (OptionalString) Schema(huma.Registry) *huma.Schema {
	minLength, maxLength := 1, 255
	return &huma.Schema{
		Type:      huma.TypeString,
		Nullable:  true,
		MinLength: &minLength,
		MaxLength: &maxLength,
	}
}

// OptionalStrings is pkg/patch.Field[[]string] with the array's OpenAPI schema.
type OptionalStrings struct {
	patch.Field[[]string]
}

// UnmarshalJSON delegates presence/null tracking to patch.Field.
func (o *OptionalStrings) UnmarshalJSON(data []byte) error { return o.Field.UnmarshalJSON(data) }

// Schema declares the member as a nullable string array.
func (OptionalStrings) Schema(huma.Registry) *huma.Schema {
	minItems, maxItems := 1, 100
	return &huma.Schema{
		Type:     huma.TypeArray,
		Nullable: true,
		Items:    &huma.Schema{Type: huma.TypeString},
		MinItems: &minItems,
		MaxItems: &maxItems,
	}
}

// APIKeyResponse is safe to return repeatedly: it contains no hash or secret.
type APIKeyResponse struct {
	ID                 uuid.UUID  `json:"id" doc:"API key id."`
	UserID             uuid.UUID  `json:"userId" doc:"Owning member id."`
	Name               string     `json:"name" doc:"Human-readable key name."`
	Prefix             string     `json:"prefix" doc:"Non-secret key prefix."`
	Scopes             []string   `json:"scopes" doc:"Granted permissions."`
	ExpiresAt          time.Time  `json:"expiresAt" doc:"Absolute expiry."`
	RevokedAt          *time.Time `json:"revokedAt,omitempty" doc:"Revocation time, if revoked."`
	RotatedAt          *time.Time `json:"rotatedAt,omitempty" doc:"Rotation time, if replaced."`
	RotationGraceUntil *time.Time `json:"rotationGraceUntil,omitempty" doc:"End of the predecessor's grace window."`
	LastUsedAt         *time.Time `json:"lastUsedAt,omitempty" doc:"Last successful authentication."`
	CreatedAt          time.Time  `json:"createdAt" doc:"Creation time."`
	UpdatedAt          time.Time  `json:"updatedAt" doc:"Last metadata update."`
}

// CreatedAPIKeyResponse is returned exactly once on create or rotate; Key is
// the plaintext credential and is never stored or returned again.
type CreatedAPIKeyResponse struct {
	APIKeyResponse
	Key string `json:"key" doc:"Plaintext key. Returned once and never stored."`
}

// RotatedAPIKeyResponse returns the replacement key and the grace window
// granted to the previous key. The previous key remains valid until
// PreviousKeyValidUntil unless it is explicitly revoked.
type RotatedAPIKeyResponse struct {
	CreatedAPIKeyResponse
	PreviousKeyID         uuid.UUID `json:"previousKeyId" doc:"Id of the key being replaced."`
	PreviousKeyValidUntil time.Time `json:"previousKeyValidUntil" doc:"End of the previous key's grace window."`
}
