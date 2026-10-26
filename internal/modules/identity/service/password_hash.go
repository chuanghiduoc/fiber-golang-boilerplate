package service

import (
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// hashPassword is the single password-hashing entry point: it applies NFKC
// normalization before bcrypt so a value set and a value offered with different
// Unicode spellings compare equal. There is no deployed database, so the
// normalization intentionally changes the hashing input; a hash created by an
// older build would have to be re-set.
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(validator.NormalizePassword(password)), bcryptCost)
	if err != nil {
		return "", apperror.NewInternal("failed to hash password")
	}
	return string(hash), nil
}

// comparePassword verifies an offered password against a stored hash using the
// same NFKC normalization as hashPassword. bcrypt's 72-byte limit applies to
// the normalized input, which the policy also enforces.
func comparePassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(validator.NormalizePassword(password))) == nil
}
