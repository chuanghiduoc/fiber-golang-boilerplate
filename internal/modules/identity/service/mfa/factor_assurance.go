package mfa

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// enrolledFactor is the strongest second factor a user has enrolled.
type enrolledFactor int

const (
	// enrolledNone: no confirmed TOTP factor and no passkey.
	enrolledNone enrolledFactor = iota
	// enrolledTOTP: a confirmed TOTP factor and no passkey.
	enrolledTOTP
	// enrolledPasskey: at least one passkey.
	enrolledPasskey
)

var (
	passkeyMethods      = []string{PasskeyMethodHardware, PasskeyMethodSoftware}
	secondFactorMethods = []string{AMROTP, AMRRecoveryCode, PasskeyMethodHardware, PasskeyMethodSoftware}
)

// strongestEnrolledFactor reads the user's factor state. Call it with the
// stores of the transaction that changes the factors, so the state it returns
// is the state the change applies to.
func strongestEnrolledFactor(ctx context.Context, stores Stores, userID uuid.UUID) (enrolledFactor, error) {
	passkeys, _, err := stores.Passkeys.ListByUserLimited(ctx, userID, 1)
	if err != nil {
		return enrolledNone, apperror.NewInternal("failed to load passkeys")
	}
	if len(passkeys) > 0 {
		return enrolledPasskey, nil
	}
	if _, err := stores.MFA.GetConfirmedTOTP(ctx, userID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return enrolledNone, nil
		}
		return enrolledNone, apperror.NewInternal("failed to load MFA factor")
	}
	return enrolledTOTP, nil
}

// ensureFactorChangeAssurance decides whether session may add or delete a
// passkey for a user in the enrolled state. The session must prove the
// strongest factor the user already has, and the proof must be fresh:
//
//   - no factor: any signed-in session. The caller has already proved the
//     password or a recent sign-in, and this is the only way to enroll a
//     first passkey.
//   - TOTP only: a fresh second factor of any kind.
//   - a passkey: a fresh passkey. A password and a TOTP code can be phished;
//     if they could add or delete a passkey, they would reach every operation
//     that the passkey-only gate protects.
//
// A user who lost the only passkey recovers through the platform MFA reset.
func ensureFactorChangeAssurance(enrolled enrolledFactor, session *Session, now time.Time, maxAge time.Duration) error {
	switch enrolled {
	case enrolledNone:
		return nil
	case enrolledTOTP:
		if sessionProves(session, secondFactorMethods, now, maxAge) {
			return nil
		}
		return factorAssuranceRequired("verify your second factor at POST /me/mfa/step-up before changing passkeys")
	case enrolledPasskey:
		if sessionProves(session, passkeyMethods, now, maxAge) {
			return nil
		}
		return factorAssuranceRequired("verify a passkey at POST /me/mfa/step-up before changing passkeys")
	default:
		return apperror.NewInternal("unknown enrolled factor state")
	}
}

// sessionProves reports whether session records one of methods and its second
// factor was verified no longer than maxAge ago. A non-positive maxAge keeps
// the method and verification requirements without an age bound, the same as
// the route MFA gate.
func sessionProves(session *Session, methods []string, now time.Time, maxAge time.Duration) bool {
	if session == nil || session.MFAVerifiedAt == nil {
		return false
	}
	verifiedAt := *session.MFAVerifiedAt
	if verifiedAt.After(now) {
		return false
	}
	if maxAge > 0 && now.Sub(verifiedAt) > maxAge {
		return false
	}
	return slices.ContainsFunc(session.AMR, func(method string) bool {
		return slices.Contains(methods, method)
	})
}

func factorAssuranceRequired(detail string) error {
	return apperror.NewCode(apperror.CodeMFARequired, detail)
}
