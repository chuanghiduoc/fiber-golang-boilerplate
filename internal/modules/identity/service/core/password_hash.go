package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// Argon2id parameters (OWASP Password Storage Cheat Sheet): 19 MiB of memory,
// two iterations, one lane. They are the production defaults; the struct
// below is a variable only so tests can use a cheap parameter set.
const (
	argon2MemoryKiB   uint32 = 19456
	argon2Iterations  uint32 = 2
	argon2Parallelism uint8  = 1
	argon2SaltLength         = 16
	argon2KeyLength          = 32
	argon2Version     uint32 = 0x13
)

// argon2Params are the tunable cost parameters encoded in a PHC string.
type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// passwordHashParams is production Argon2id. Package tests replace it with
// testPasswordHashParams in TestMain.
var passwordHashParams = argon2Params{
	memory:      argon2MemoryKiB,
	iterations:  argon2Iterations,
	parallelism: argon2Parallelism,
}

// testPasswordHashParams is the cheap fixture set: real Argon2id, low cost.
var testPasswordHashParams = argon2Params{memory: 1024, iterations: 1, parallelism: 1}

// HashPassword is the single password-hashing entry point: it applies NFKC
// normalization, then Argon2id, and returns a PHC string
// ($argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>).
func HashPassword(password string) (string, error) {
	input := []byte(validator.NormalizePassword(password))
	salt := make([]byte, argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", apperror.NewInternal("failed to hash password")
	}
	return encodeArgon2id(input, salt, passwordHashParams), nil
}

// VerifyPasswordHash reports whether password matches a stored PHC hash. It is
// the exported counterpart used by integration tests and tooling.
func VerifyPasswordHash(hash, password string) bool {
	ok, _ := VerifyPassword(hash, password)
	return ok
}

func encodeArgon2id(password, salt []byte, params argon2Params) string {
	key := argon2.IDKey(password, salt, params.iterations, params.memory, params.parallelism, argon2KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, params.memory, params.iterations, params.parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// VerifyPassword checks an offered password against a stored PHC hash and
// reports whether the hash uses the current parameters and should be rehashed
// after a successful verification.
func VerifyPassword(hash, password string) (ok, needsRehash bool) {
	params, salt, key, err := parseArgon2id(hash)
	if err != nil {
		return false, false
	}
	derived := argon2.IDKey([]byte(validator.NormalizePassword(password)), salt,
		params.iterations, params.memory, params.parallelism, argon2KeyLength)
	if subtle.ConstantTimeCompare(derived, key) != 1 {
		return false, false
	}
	return true, params != passwordHashParams || len(salt) != argon2SaltLength
}

// parseArgon2id decodes a PHC Argon2id string.
func parseArgon2id(hash string) (params argon2Params, salt, key []byte, err error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params, nil, nil, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != int(argon2Version) {
		return params, nil, nil, errors.New("unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallelism); err != nil {
		return params, nil, nil, errors.New("invalid argon2 parameters")
	}
	if params.memory == 0 || params.iterations == 0 || params.parallelism == 0 {
		return params, nil, nil, errors.New("invalid argon2 parameters")
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return params, nil, nil, errors.New("invalid argon2 salt")
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argon2KeyLength {
		return params, nil, nil, errors.New("invalid argon2 hash")
	}
	return params, salt, key, nil
}

// dummyPasswordHash equalizes Argon2id work for unknown-email logins so the
// response time does not reveal whether an account exists. It uses the same
// parameters as every stored hash.
var dummyPasswordHash = func() string {
	hash, err := HashPassword("timing-only-dummy-password")
	if err != nil {
		panic("identity: build dummy password hash: " + err.Error())
	}
	return hash
}()

// spendDummyPasswordWork performs one full verification against the dummy
// hash, returning the same work as a real failed comparison.
func SpendDummyPasswordWork(password string) {
	_, _ = VerifyPassword(dummyPasswordHash, password)
}

// SetPasswordHashParamsForTest installs a cheap Argon2id parameter set for
// package tests outside this package.
func SetPasswordHashParamsForTest(memory, iterations uint32, parallelism uint8) {
	passwordHashParams = argon2Params{memory: memory, iterations: iterations, parallelism: parallelism}
}

// SetDummyPasswordHashForTest rebuilds the timing-equalizing dummy hash after
// the test parameter set changed.
func SetDummyPasswordHashForTest(hash string) { dummyPasswordHash = hash }

// Exported cost constants for tests and tooling outside this package.
const (
	Argon2MemoryKiB   = argon2MemoryKiB
	Argon2Iterations  = argon2Iterations
	Argon2Parallelism = argon2Parallelism
	Argon2SaltLength  = argon2SaltLength
	Argon2KeyLength   = argon2KeyLength
	Argon2Version     = argon2Version
)

// EncodeArgon2idWithParams builds a PHC hash with explicit cost parameters.
// It is used by tests that need to forge an older-parameter hash.
func EncodeArgon2idWithParams(password string, memory, iterations uint32, parallelism uint8) string {
	return encodeArgon2id([]byte(validator.NormalizePassword(password)),
		[]byte("0123456789abcdef"), argon2Params{memory: memory, iterations: iterations, parallelism: parallelism})
}
