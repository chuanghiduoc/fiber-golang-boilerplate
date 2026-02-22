package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type RefreshTokenService interface {
	Create(ctx context.Context, userID int64) (string, error)
	Verify(ctx context.Context, token string) (*sqlc.RefreshToken, error)
	Revoke(ctx context.Context, token string) error
	RevokeAllByUserID(ctx context.Context, userID int64) error
}

type refreshTokenService struct {
	repo       repository.RefreshTokenRepository
	expireDays int
}

func NewRefreshTokenService(repo repository.RefreshTokenRepository, expireDays int) RefreshTokenService {
	return &refreshTokenService{repo: repo, expireDays: expireDays}
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *refreshTokenService) Create(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", apperror.NewInternal("failed to generate refresh token")
	}
	plainToken := hex.EncodeToString(b)

	expiresAt := time.Now().Add(time.Duration(s.expireDays) * 24 * time.Hour)

	_, err := s.repo.Create(ctx, sqlc.CreateRefreshTokenParams{
		UserID:    userID,
		Token:     hashToken(plainToken), // Store hash, not plaintext
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return "", apperror.NewInternal("failed to store refresh token")
	}

	return plainToken, nil // Return plaintext to client
}

func (s *refreshTokenService) Verify(ctx context.Context, token string) (*sqlc.RefreshToken, error) {
	rt, err := s.repo.GetByToken(ctx, hashToken(token)) // Lookup by hash
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewUnauthorized("invalid refresh token")
		}
		return nil, apperror.NewInternal("failed to verify refresh token")
	}

	if rt.ExpiresAt.Time.Before(time.Now()) {
		_ = s.repo.Delete(ctx, hashToken(token))
		return nil, apperror.NewUnauthorized("refresh token expired")
	}

	return rt, nil
}

func (s *refreshTokenService) Revoke(ctx context.Context, token string) error {
	return s.repo.Delete(ctx, hashToken(token)) // Delete by hash
}

func (s *refreshTokenService) RevokeAllByUserID(ctx context.Context, userID int64) error {
	return s.repo.DeleteByUserID(ctx, userID)
}
