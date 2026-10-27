// Package softauthenticator builds WebAuthn registration and assertion
// responses with a software P-256 key. It is a test fixture: no private key
// ever leaves the process, and the package is imported only by tests.
package softauthenticator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// Authenticator is one software WebAuthn authenticator. Its credential id and
// key live for the life of the test.
type Authenticator struct {
	RPID           string
	Origin         string
	CredentialID   []byte
	SignCount      uint32
	BackupEligible bool
	BackupState    bool
	AAGUID         [16]byte

	key *ecdsa.PrivateKey
}

// credentialIDBytes is the fixed fixture credential id length. The AT
// credential-id length prefix is therefore the constant pair below; computing
// it from len() would require a checked integer conversion for no benefit in a
// test fixture.
const credentialIDBytes = 32

// New creates a software authenticator for rpID/origin.
func New(t *testing.T, rpID, origin string) *Authenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate authenticator key: %v", err)
	}
	credentialID := make([]byte, credentialIDBytes)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatalf("generate credential id: %v", err)
	}
	aaguid := [16]byte{}
	if _, err := rand.Read(aaguid[:]); err != nil {
		t.Fatalf("generate aaguid: %v", err)
	}
	return &Authenticator{
		RPID: rpID, Origin: origin, CredentialID: credentialID, AAGUID: aaguid, key: key,
	}
}

// Attestation returns a "none"-attestation registration response for challenge.
func (a *Authenticator) Attestation(t *testing.T, challenge string) []byte {
	t.Helper()
	clientData := a.clientData(t, "webauthn.create", challenge)

	rpIDHash := sha256.Sum256([]byte(a.RPID))
	flags := byte(0x01 | 0x04 | 0x40) // UP | UV | AT
	flags = a.backupFlags(flags)
	authData := make([]byte, 0, 37+16+2+len(a.CredentialID)+128)
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, a.SignCount)
	authData = append(authData, a.AAGUID[:]...)
	authData = append(authData, 0x00, credentialIDBytes) // uint16 big-endian credential id length
	authData = append(authData, a.CredentialID...)
	cose, err := cbor.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: a.key.X.FillBytes(make([]byte, 32)),
		-3: a.key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("encode credential public key: %v", err)
	}
	authData = append(authData, cose...)

	attestationObject, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		t.Fatalf("encode attestation object: %v", err)
	}
	return a.response(t, map[string]any{
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
		"attestationObject": base64.RawURLEncoding.EncodeToString(attestationObject),
		"transports":        []string{"internal"},
	})
}

// Assertion returns an assertion response and advances the counter by one.
func (a *Authenticator) Assertion(t *testing.T, challenge string, userHandle []byte) []byte {
	t.Helper()
	a.SignCount++
	return a.AssertionWithCounter(t, challenge, userHandle, a.SignCount)
}

// AssertionWithCounter returns an assertion with an explicit signature counter,
// used to prove that a regression is rejected.
func (a *Authenticator) AssertionWithCounter(t *testing.T, challenge string, userHandle []byte, counter uint32) []byte {
	t.Helper()
	clientData := a.clientData(t, "webauthn.get", challenge)
	clientDataHash := sha256.Sum256(clientData)

	rpIDHash := sha256.Sum256([]byte(a.RPID))
	flags := byte(0x01 | 0x04) // UP | UV
	flags = a.backupFlags(flags)
	authData := make([]byte, 0, 37)
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)

	digest := sha256.Sum256(append(append([]byte(nil), authData...), clientDataHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return a.response(t, map[string]any{
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
		"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
		"signature":         base64.RawURLEncoding.EncodeToString(signature),
		"userHandle":        base64.RawURLEncoding.EncodeToString(userHandle),
	})
}

func (a *Authenticator) clientData(t *testing.T, ceremonyType, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":        ceremonyType,
		"challenge":   challenge,
		"origin":      a.Origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatalf("encode client data: %v", err)
	}
	return data
}

func (a *Authenticator) response(t *testing.T, response map[string]any) []byte {
	t.Helper()
	encodedID := base64.RawURLEncoding.EncodeToString(a.CredentialID)
	credential := map[string]any{
		"id":                      encodedID,
		"rawId":                   encodedID,
		"type":                    "public-key",
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "platform",
		"response":                response,
	}
	data, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	return data
}

func (a *Authenticator) backupFlags(flags byte) byte {
	if a.BackupEligible {
		flags |= 0x08
	}
	if a.BackupState {
		flags |= 0x10
	}
	return flags
}
