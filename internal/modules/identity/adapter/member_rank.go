package adapter

import (
	"context"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/google/uuid"
)

// MemberRank applies identity's tenant role hierarchy to a caller managing
// another member, without exposing identity's repositories to the caller.
type MemberRank struct {
	stores identityservice.Stores
	tx     identityservice.Transactor
}

var _ identitycontracts.MemberRankChecker = (*MemberRank)(nil)

func NewMemberRank(stores identityservice.Stores, tx identityservice.Transactor) *MemberRank {
	return &MemberRank{stores: stores, tx: tx}
}

// CanManageMember runs identity's rank check in its own transaction so the
// target membership row is locked for the duration of the check.
func (m *MemberRank) CanManageMember(ctx context.Context, targetUserID uuid.UUID) error {
	return m.tx.InTx(ctx, func(ctx context.Context, tx identityservice.TxRepos) error {
		return identityservice.EnsureCanManageMember(ctx, tx.Stores, targetUserID)
	})
}
