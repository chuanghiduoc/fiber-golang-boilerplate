package dto

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PasskeyCredential is the browser's PublicKeyCredential in JSON form. The
// response member is passed verbatim to the WebAuthn parser, so this type never
// interprets the attestation or assertion itself.
type PasskeyCredential struct {
	ID                      string          `json:"id" required:"true" doc:"Base64url credential id."`
	RawID                   string          `json:"rawId" required:"true" doc:"Base64url raw credential id."`
	Type                    string          `json:"type" required:"true" enum:"public-key" doc:"Credential type."`
	AuthenticatorAttachment string          `json:"authenticatorAttachment,omitempty" doc:"platform or cross-platform."`
	ClientExtensionResults  json.RawMessage `json:"clientExtensionResults,omitempty" doc:"Client extension outputs."`
	Response                json.RawMessage `json:"response" required:"true" doc:"Authenticator attestation or assertion response."`
}

// MarshalCredential re-encodes the credential without the API envelope fields
// for the WebAuthn parser.
func (c PasskeyCredential) MarshalCredential() ([]byte, error) {
	return json.Marshal(struct {
		ID                      string          `json:"id"`
		RawID                   string          `json:"rawId"`
		Type                    string          `json:"type"`
		AuthenticatorAttachment string          `json:"authenticatorAttachment,omitempty"`
		ClientExtensionResults  json.RawMessage `json:"clientExtensionResults,omitempty"`
		Response                json.RawMessage `json:"response"`
	}{
		ID:                      c.ID,
		RawID:                   c.RawID,
		Type:                    c.Type,
		AuthenticatorAttachment: c.AuthenticatorAttachment,
		ClientExtensionResults:  c.ClientExtensionResults,
		Response:                c.Response,
	})
}

// PasskeyRegistrationRequest carries a registration attestation. The credential
// may be sent either in the envelope's "credential" member or as the body
// itself (the browser's PublicKeyCredential shape); name remains optional.
type PasskeyRegistrationRequest struct {
	Name       string            `json:"name,omitempty" maxLength:"64" doc:"Label shown in the caller's passkey list."`
	Credential PasskeyCredential `json:"credential" doc:"Registration attestation."`
}

func (r *PasskeyRegistrationRequest) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	r.Name = envelope.Name
	if len(envelope.Credential) > 0 && string(envelope.Credential) != "null" {
		return json.Unmarshal(envelope.Credential, &r.Credential)
	}
	return json.Unmarshal(data, &r.Credential)
}

// PasskeyLoginOptionsRequest optionally hints at the account. The hint only
// narrows what the assertion must resolve to; the options never reveal whether
// the address exists and always allow a discoverable credential.
type PasskeyLoginOptionsRequest struct {
	Email string `json:"email,omitempty" format:"email" maxLength:"255"`
}

// PasskeyLoginVerificationRequest carries a login assertion. Like registration,
// the credential may be sent as the whole body or in the "credential" member.
type PasskeyLoginVerificationRequest struct {
	Credential PasskeyCredential `json:"credential" doc:"Authentication assertion."`
}

func (r *PasskeyLoginVerificationRequest) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if len(envelope.Credential) > 0 && string(envelope.Credential) != "null" {
		return json.Unmarshal(envelope.Credential, &r.Credential)
	}
	return json.Unmarshal(data, &r.Credential)
}

// PasskeyResponse is the public view of a passkey. It never includes the
// credential public key or any other key material.
type PasskeyResponse struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Transports     []string   `json:"transports"`
	BackupEligible bool       `json:"backupEligible"`
	BackupState    bool       `json:"backupState"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}
