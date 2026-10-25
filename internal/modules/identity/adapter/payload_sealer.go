package adapter

import (
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

// PayloadSealer adapts jobs.PayloadCodec (DATA_ENCRYPTION_KEY and its previous
// keys) to the identity service's SecretSealer port, so TOTP secrets and login
// challenges use the same keyring as sealed job payloads.
type PayloadSealer struct {
	codec *jobs.PayloadCodec
}

var _ identityservice.SecretSealer = (*PayloadSealer)(nil)

func NewPayloadSealer(codec *jobs.PayloadCodec) *PayloadSealer {
	return &PayloadSealer{codec: codec}
}

func (s *PayloadSealer) Seal(plaintext []byte) ([]byte, error) {
	return s.codec.Seal(plaintext)
}

func (s *PayloadSealer) Open(sealed []byte) ([]byte, error) {
	return s.codec.Open(sealed)
}
