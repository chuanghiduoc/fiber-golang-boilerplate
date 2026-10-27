package core

import (
	"math"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnCredential reconstructs the library record needed to verify an
// assertion. Only the fields WebAuthn signature verification reads are stored,
// so no attestation blob is persisted.
func (p *Passkey) WebAuthnCredential() webauthn.Credential {
	signCount := uint32(0)
	if p.SignCount > 0 && p.SignCount <= math.MaxUint32 {
		signCount = uint32(p.SignCount)
	}
	authenticator := webauthn.Authenticator{SignCount: signCount}
	if p.AAGUID.Valid {
		aaguid := p.AAGUID.UUID
		authenticator.AAGUID = aaguid[:]
	}
	transports := make([]protocol.AuthenticatorTransport, len(p.Transports))
	for i, transport := range p.Transports {
		transports[i] = protocol.AuthenticatorTransport(transport)
	}
	return webauthn.Credential{
		ID:        p.CredentialID,
		PublicKey: p.PublicKey,
		Transport: transports,
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   true,
			BackupEligible: p.BackupEligible,
			BackupState:    p.BackupState,
		},
		Authenticator: authenticator,
	}
}
