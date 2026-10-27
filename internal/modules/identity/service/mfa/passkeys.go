package mfa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	// PasskeyCeremonyTTL bounds a WebAuthn challenge. A ceremony is single use
	// and is consumed before its assertion/attestation is verified.
	PasskeyCeremonyTTL = 5 * time.Minute

	passkeyCeremonyVersion      = 1
	passkeyCeremonyRegistration = "registration"
	passkeyCeremonyLogin        = "login"
	passkeyCeremonyKeyPrefix    = "identity:passkey:ceremony:"
	passkeyDefaultName          = "Passkey"
	passkeyMaxNameLen           = 64
)

// PasskeyLogin is a completed passkey sign-in.
type PasskeyLogin struct {
	Tokens TokenPair
	Member *Member
	// Method is the RFC 8176 amr value recorded for this authentication:
	// hwk (hardware-bound) or swk (synced/backup-eligible).
	Method string
}

// PasskeyService runs the WebAuthn registration and login ceremonies and
// manages a global user's passkeys. All ceremonies are short-lived, single-use,
// tenant-bound challenges sealed with the data-encryption codec and stored in
// the cache, the same pattern as the MFA login challenge.
type PasskeyService interface {
	BeginRegistration(ctx context.Context, userID uuid.UUID) (*protocol.CredentialCreation, error)
	FinishRegistration(ctx context.Context, userID uuid.UUID, name string, raw []byte) (*Passkey, error)
	Delete(ctx context.Context, userID, passkeyID uuid.UUID) error
	List(ctx context.Context, userID uuid.UUID) ([]Passkey, error)
	BeginLogin(ctx context.Context, email string) (*protocol.CredentialAssertion, error)
	FinishLogin(ctx context.Context, raw []byte, admit Admission) (*PasskeyLogin, error)
	// StepUpPasskey verifies a passkey assertion for the signed-in user and
	// refreshes the caller's session, adding hwk/swk to its amr.
	StepUpPasskey(ctx context.Context, userID, sessionID uuid.UUID, raw []byte) (time.Time, error)
}

// PasskeyDeps are the passkey service's collaborators.
type PasskeyDeps struct {
	Stores   Stores
	Tx       Transactor
	Cache    cache.Cache
	Sealer   SecretSealer
	Sessions *SessionService
	WebAuthn *webauthn.WebAuthn
	Security *SecurityNotifier
	// Now is a test seam; nil uses time.Now.
	Now func() time.Time
}

type passkeyService struct {
	PasskeyDeps
}

var _ PasskeyService = (*passkeyService)(nil)

// NewPasskeyService wires the passkey service. WebAuthn may be nil when
// passkeys are disabled; every ceremony then fails closed.
func NewPasskeyService(deps PasskeyDeps) PasskeyService {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &passkeyService{PasskeyDeps: deps}
}

func (s *passkeyService) now() time.Time { return s.Now() }

func passkeyInvalid() error {
	return apperror.NewCode(identitycatalog.CodeAuthPasskeyInvalid, "invalid or expired passkey ceremony")
}

func passkeySignCountInvalid() error {
	return apperror.NewCode(identitycatalog.CodeAuthPasskeySignCount,
		"the authenticator signature counter did not advance")
}

func passkeyNotConfigured() error {
	return apperror.NewCode(apperror.CodeSystemUnavailable, "passkeys are not configured")
}

// passkeyStoreUnavailable fails a ceremony when the challenge cache cannot be
// read or written; it must never be mistaken for an invalid assertion.
func passkeyStoreUnavailable(err error) error {
	slog.Error("passkey ceremony store unavailable", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemUnavailable, "unable to verify the passkey ceremony, please try again later")
}

// passkeyCeremony is the sealed state between the options and verification
// calls. It carries only data the server re-validates.
type passkeyCeremony struct {
	Version   int                  `json:"v"`
	Kind      string               `json:"kind"`
	TenantID  uuid.UUID            `json:"tid"`
	UserID    uuid.UUID            `json:"uid,omitempty"`
	EmailHint string               `json:"email,omitempty"`
	Session   webauthn.SessionData `json:"session"`
	ExpiresAt int64                `json:"exp"`
}

func (c *passkeyCeremony) validate(tenantID uuid.UUID, kind string, userID uuid.UUID, now time.Time) error {
	if c.Version != passkeyCeremonyVersion || c.TenantID != tenantID || c.Kind != kind || c.Session.Challenge == "" {
		return passkeyInvalid()
	}
	if kind == passkeyCeremonyRegistration && c.UserID != userID {
		return passkeyInvalid()
	}
	if !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return passkeyInvalid()
	}
	return nil
}

func (s *passkeyService) storeCeremony(ctx context.Context, ceremony passkeyCeremony) error {
	if s.Cache == nil || s.Sealer == nil {
		return passkeyNotConfigured()
	}
	payload, err := json.Marshal(ceremony)
	if err != nil {
		return apperror.NewInternal("failed to encode passkey ceremony")
	}
	sealed, err := s.Sealer.Seal(payload)
	if err != nil {
		return apperror.NewInternal("failed to seal passkey ceremony")
	}
	stored, err := s.Cache.SetNX(ctx, passkeyCeremonyKey(ceremony.TenantID, ceremony.Session.Challenge), sealed, PasskeyCeremonyTTL)
	if err != nil {
		return passkeyStoreUnavailable(err)
	}
	if !stored {
		return apperror.NewInternal("failed to store passkey ceremony")
	}
	return nil
}

// consumeCeremony opens and deletes a ceremony in one atomic compare-and-delete.
// It runs before cryptographic verification, so a failed or replayed response
// cannot be retried with the same challenge; the client requests new options.
func (s *passkeyService) consumeCeremony(ctx context.Context, challenge, kind string, userID uuid.UUID) (*passkeyCeremony, error) {
	if challenge == "" {
		return nil, passkeyInvalid()
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, apperror.NewInternal("tenant context is required")
	}
	if s.Cache == nil || s.Sealer == nil {
		return nil, passkeyNotConfigured()
	}
	key := passkeyCeremonyKey(tenantID, challenge)
	sealed, err := s.Cache.Get(ctx, key)
	if err != nil {
		return nil, passkeyStoreUnavailable(err)
	}
	if len(sealed) == 0 {
		return nil, passkeyInvalid()
	}
	plain, err := s.Sealer.Open(sealed)
	if err != nil {
		return nil, passkeyInvalid()
	}
	var ceremony passkeyCeremony
	if err := json.Unmarshal(plain, &ceremony); err != nil {
		return nil, passkeyInvalid()
	}
	if err := ceremony.validate(tenantID, kind, userID, s.now()); err != nil {
		return nil, err
	}
	compareDelete, ok := s.Cache.(cache.CompareDeleter)
	if !ok {
		return nil, passkeyStoreUnavailable(errors.New("passkey ceremony store cannot compare-and-delete"))
	}
	consumed, err := compareDelete.CompareAndDelete(ctx, key, sealed)
	if err != nil {
		return nil, passkeyStoreUnavailable(err)
	}
	if !consumed {
		return nil, passkeyInvalid()
	}
	return &ceremony, nil
}

func passkeyCeremonyKey(tenantID uuid.UUID, challenge string) string {
	sum := sha256.Sum256([]byte(challenge))
	return fmt.Sprintf("%s%s:%s", passkeyCeremonyKeyPrefix, tenantID, hex.EncodeToString(sum[:]))
}

// passkeyUser adapts an identity user and its credentials to webauthn.User.
type passkeyUser struct {
	id          []byte
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func passkeyUserID(id uuid.UUID) []byte {
	value := id
	return value[:]
}

func toPasskeyUser(user *User, passkeys []Passkey) *passkeyUser {
	credentials := make([]webauthn.Credential, 0, len(passkeys))
	for i := range passkeys {
		credentials = append(credentials, passkeys[i].WebAuthnCredential())
	}
	return &passkeyUser{
		id:          passkeyUserID(user.ID),
		name:        user.Email,
		displayName: user.Name,
		credentials: credentials,
	}
}

func (s *passkeyService) BeginRegistration(ctx context.Context, userID uuid.UUID) (*protocol.CredentialCreation, error) {
	if s.WebAuthn == nil {
		return nil, passkeyNotConfigured()
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, apperror.NewInternal("tenant context is required")
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	passkeys, err := s.Stores.Passkeys.ListByUser(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load passkeys")
	}
	entity := toPasskeyUser(user, passkeys)
	creation, session, err := s.WebAuthn.BeginRegistration(
		entity,
		webauthn.WithExclusions(webauthn.Credentials(entity.credentials).CredentialDescriptors()),
	)
	if err != nil {
		return nil, apperror.NewInternal("failed to start passkey registration")
	}
	if err := s.storeCeremony(ctx, passkeyCeremony{
		Version:   passkeyCeremonyVersion,
		Kind:      passkeyCeremonyRegistration,
		TenantID:  tenantID,
		UserID:    userID,
		Session:   *session,
		ExpiresAt: s.now().Add(PasskeyCeremonyTTL).Unix(),
	}); err != nil {
		return nil, err
	}
	return creation, nil
}

func (s *passkeyService) FinishRegistration(ctx context.Context, userID uuid.UUID, name string, raw []byte) (*Passkey, error) {
	if s.WebAuthn == nil {
		return nil, passkeyNotConfigured()
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return nil, passkeyInvalid()
	}
	ceremony, err := s.consumeCeremony(ctx, parsed.Response.CollectedClientData.Challenge, passkeyCeremonyRegistration, userID)
	if err != nil {
		return nil, err
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, passkeyInvalid()
	}
	passkeys, err := s.Stores.Passkeys.ListByUser(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load passkeys")
	}
	credential, err := s.WebAuthn.CreateCredential(toPasskeyUser(user, passkeys), ceremony.Session, parsed)
	if err != nil {
		return nil, passkeyInvalid()
	}
	normalized, err := normalizePasskeyName(name)
	if err != nil {
		return nil, err
	}
	passkey := Passkey{
		UserID:         userID,
		CredentialID:   credential.ID,
		PublicKey:      credential.PublicKey,
		SignCount:      int64(credential.Authenticator.SignCount),
		AAGUID:         passkeyAAGUID(credential.Authenticator.AAGUID),
		Transports:     passkeyTransports(credential.Transport),
		BackupEligible: credential.Flags.BackupEligible,
		BackupState:    credential.Flags.BackupState,
		Name:           normalized,
	}
	var result *Passkey
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		created, err := tx.Passkeys.Create(ctx, passkey)
		if err != nil {
			if errors.Is(err, ErrPasskeyAlreadyRegistered) {
				return apperror.NewCode(identitycatalog.CodeAuthPasskeyAlreadyRegistered,
					"this passkey is already registered")
			}
			return apperror.NewInternal("failed to store passkey")
		}
		if err := s.Security.Deliver(ctx, tx, SecurityNotice{
			Event:       SecurityEventPasskeyAdded,
			UserID:      user.ID,
			Name:        user.Name,
			Email:       user.Email,
			PasskeyName: created.Name,
			OccurredAt:  s.now().UTC(),
		}); err != nil {
			return err
		}
		if err := checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.passkey_registered", userID, map[string]any{
			"passkey_id":      created.ID.String(),
			"backup_eligible": created.BackupEligible,
			"backup_state":    created.BackupState,
			"transports":      created.Transports,
		}))); err != nil {
			return err
		}
		result = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *passkeyService) Delete(ctx context.Context, userID, passkeyID uuid.UUID) error {
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to load user")
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		passkey, err := tx.Passkeys.Get(ctx, userID, passkeyID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewCode(identitycatalog.CodeAuthPasskeyNotFound, "passkey not found")
			}
			return apperror.NewInternal("failed to load passkey")
		}
		deleted, err := tx.Passkeys.Delete(ctx, userID, passkeyID)
		if err != nil {
			return apperror.NewInternal("failed to delete passkey")
		}
		if !deleted {
			return apperror.NewCode(identitycatalog.CodeAuthPasskeyNotFound, "passkey not found")
		}
		if err := s.Security.Deliver(ctx, tx, SecurityNotice{
			Event:       SecurityEventPasskeyRemoved,
			UserID:      user.ID,
			Name:        user.Name,
			Email:       user.Email,
			PasskeyName: passkey.Name,
			OccurredAt:  s.now().UTC(),
		}); err != nil {
			return err
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.passkey_deleted", userID, map[string]any{
			"passkey_id": passkey.ID.String(),
		})))
	})
	return err
}

func (s *passkeyService) List(ctx context.Context, userID uuid.UUID) ([]Passkey, error) {
	passkeys, err := s.Stores.Passkeys.ListByUser(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to list passkeys")
	}
	return passkeys, nil
}

func (s *passkeyService) BeginLogin(ctx context.Context, email string) (*protocol.CredentialAssertion, error) {
	if s.WebAuthn == nil {
		return nil, passkeyNotConfigured()
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, apperror.NewInternal("tenant context is required")
	}
	assertion, session, err := s.WebAuthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, apperror.NewInternal("failed to start passkey login")
	}
	if err := s.storeCeremony(ctx, passkeyCeremony{
		Version:   passkeyCeremonyVersion,
		Kind:      passkeyCeremonyLogin,
		TenantID:  tenantID,
		EmailHint: NormalizeEmail(email),
		Session:   *session,
		ExpiresAt: s.now().Add(PasskeyCeremonyTTL).Unix(),
	}); err != nil {
		return nil, err
	}
	return assertion, nil
}

// assertionIdentity is the resolved owner of an asserted credential.
type assertionIdentity struct {
	passkey *Passkey
	user    *User
	member  *Member
	userWeb *passkeyUser
}

// resolveAssertion looks up the credential owner and checks the discoverable
// user handle, the optional email hint, and the active membership. The returned
// error is intentionally generic: a caller must not learn whether a credential
// id or an account exists.
func (s *passkeyService) resolveAssertion(ctx context.Context, emailHint string, rawID, userHandle []byte) (*assertionIdentity, error) {
	passkey, err := s.Stores.Passkeys.GetByCredentialID(ctx, rawID)
	if err != nil {
		return nil, passkeyInvalid()
	}
	if !bytes.Equal(userHandle, passkeyUserID(passkey.UserID)) {
		return nil, passkeyInvalid()
	}
	user, err := s.Stores.Users.GetByID(ctx, passkey.UserID)
	if err != nil {
		return nil, passkeyInvalid()
	}
	if emailHint != "" && !strings.EqualFold(emailHint, user.Email) {
		return nil, passkeyInvalid()
	}
	member, err := s.Stores.Memberships.GetActive(ctx, user.ID)
	if err != nil {
		return nil, passkeyInvalid()
	}
	passkeys, err := s.Stores.Passkeys.ListByUser(ctx, user.ID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load passkeys")
	}
	return &assertionIdentity{
		passkey: passkey,
		user:    user,
		member:  member,
		userWeb: toPasskeyUser(user, passkeys),
	}, nil
}

func (s *passkeyService) FinishLogin(ctx context.Context, raw []byte, admit Admission) (*PasskeyLogin, error) {
	if s.WebAuthn == nil {
		return nil, passkeyNotConfigured()
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, passkeyInvalid()
	}
	ceremony, err := s.consumeCeremony(ctx, parsed.Response.CollectedClientData.Challenge, passkeyCeremonyLogin, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var identity *assertionIdentity
	_, credential, err := s.WebAuthn.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			resolved, err := s.resolveAssertion(ctx, ceremony.EmailHint, rawID, userHandle)
			if err != nil {
				return nil, err
			}
			identity = resolved
			return resolved.userWeb, nil
		},
		ceremony.Session, parsed,
	)
	if err != nil || identity == nil {
		return nil, passkeyInvalid()
	}
	if credential.Authenticator.CloneWarning {
		return nil, passkeySignCountInvalid()
	}
	if admit != nil {
		if err := admit(ctx, identity.member); err != nil {
			return nil, err
		}
	}
	method := identity.passkey.Method()
	var result *PasskeyLogin
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		advanced, err := tx.Passkeys.UpdateAfterAssertion(ctx, identity.user.ID, identity.passkey.ID,
			int64(credential.Authenticator.SignCount), credential.Flags.BackupState)
		if err != nil {
			return apperror.NewInternal("failed to update passkey")
		}
		if !advanced {
			return passkeySignCountInvalid()
		}
		tokens, err := s.Sessions.IssueTx(ctx, tx.Stores, identity.user.ID, identity.user.TokenVersion, []string{method})
		if err != nil {
			return err
		}
		member, err := tx.Memberships.GetActive(ctx, identity.user.ID)
		if err != nil {
			return accountNotActive()
		}
		event := userEvent(ctx, "auth.passkey_login", identity.user.ID, map[string]any{
			"method":     method,
			"passkey_id": identity.passkey.ID.String(),
		})
		event.TenantID = member.Membership.TenantID
		event.ActorID = identity.user.ID
		event.ActorRole = member.Membership.Role
		event.ActorType = audit.ActorTypeUser
		if err := checkAudit(tx.Audit.RecordTx(ctx, event)); err != nil {
			return err
		}
		result = &PasskeyLogin{Tokens: tokens, Member: member, Method: method}
		return nil
	})
	if err != nil {
		return nil, err
	}
	clearLoginAttempts(ctx, s.Cache, identity.user.Email)
	return result, nil
}

func (s *passkeyService) StepUpPasskey(ctx context.Context, userID, sessionID uuid.UUID, raw []byte) (time.Time, error) {
	if s.WebAuthn == nil {
		return time.Time{}, passkeyNotConfigured()
	}
	if sessionID == uuid.Nil {
		return time.Time{}, apperror.NewUnauthorized("session is required")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return time.Time{}, passkeyInvalid()
	}
	ceremony, err := s.consumeCeremony(ctx, parsed.Response.CollectedClientData.Challenge, passkeyCeremonyLogin, uuid.Nil)
	if err != nil {
		return time.Time{}, err
	}
	var identity *assertionIdentity
	_, credential, err := s.WebAuthn.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			resolved, err := s.resolveAssertion(ctx, ceremony.EmailHint, rawID, userHandle)
			if err != nil {
				return nil, err
			}
			if resolved.user.ID != userID {
				return nil, passkeyInvalid()
			}
			identity = resolved
			return resolved.userWeb, nil
		},
		ceremony.Session, parsed,
	)
	if err != nil || identity == nil {
		return time.Time{}, passkeyInvalid()
	}
	if credential.Authenticator.CloneWarning {
		return time.Time{}, passkeySignCountInvalid()
	}
	method := identity.passkey.Method()
	var verifiedAt time.Time
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		advanced, err := tx.Passkeys.UpdateAfterAssertion(ctx, identity.user.ID, identity.passkey.ID,
			int64(credential.Authenticator.SignCount), credential.Flags.BackupState)
		if err != nil {
			return apperror.NewInternal("failed to update passkey")
		}
		if !advanced {
			return passkeySignCountInvalid()
		}
		ts, err := tx.Sessions.MarkMFAVerifiedWithMethod(ctx, userID, sessionID, method)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewUnauthorized("session is no longer valid")
			}
			return apperror.NewInternal("failed to refresh MFA verification")
		}
		verifiedAt = ts
		event := userEvent(ctx, "auth.mfa_step_up", userID, map[string]any{
			"method":     method,
			"passkey_id": identity.passkey.ID.String(),
		})
		event.ActorID = userID
		event.ActorType = audit.ActorTypeUser
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		return time.Time{}, err
	}
	if s.Sessions != nil {
		s.Sessions.Invalidate(ctx, userID)
	}
	return verifiedAt, nil
}

func normalizePasskeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return passkeyDefaultName, nil
	}
	if utf8.RuneCountInString(name) > passkeyMaxNameLen {
		return "", apperror.NewCode(identitycatalog.CodeAuthPasskeyNameInvalid,
			"passkey name must be at most 64 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperror.NewCode(identitycatalog.CodeAuthPasskeyNameInvalid,
				"passkey name must not contain control characters")
		}
	}
	return name, nil
}

func passkeyAAGUID(raw []byte) uuid.NullUUID {
	if len(raw) != 16 {
		return uuid.NullUUID{}
	}
	id, err := uuid.FromBytes(raw)
	if err != nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: id, Valid: true}
}

// knownPasskeyTransports mirrors the table's CHECK constraint. A client may
// report a transport this build does not know (for example one added by a
// newer WebAuthn level); it is dropped rather than turning a valid
// registration into a database error.
var knownPasskeyTransports = map[string]struct{}{
	"usb": {}, "nfc": {}, "ble": {}, "internal": {}, "hybrid": {}, "smart-card": {},
}

func passkeyTransports(raw []protocol.AuthenticatorTransport) []string {
	out := make([]string, 0, len(raw))
	for _, transport := range raw {
		value := string(transport)
		if _, ok := knownPasskeyTransports[value]; !ok {
			continue
		}
		out = append(out, value)
	}
	return out
}

// PasskeyUserID is the stable WebAuthn user handle for a user id.
func PasskeyUserID(id uuid.UUID) []byte { return passkeyUserID(id) }
