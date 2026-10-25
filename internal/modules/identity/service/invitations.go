package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// invitationTTL is how long an emailed invitation link stays valid.
const invitationTTL = 7 * 24 * time.Hour

// InviteInput is a tenant admin's invitation of an address into the request
// tenant.
type InviteInput struct {
	InviterID int64
	Email     string
	Role      string
}

// InvitationSignUp creates an account from an invitation.
type InvitationSignUp struct {
	Token    string
	Name     string
	Password string
}

// InvitationService invites people into tenants. A global user joins another
// tenant only by accepting an invitation (ADR 0004); registering again with
// the same address is refused with user.email_taken.
type InvitationService struct {
	stores      Stores
	tx          Transactor
	sender      email.Sender
	frontendURL string
}

// NewInvitationService wires the invitation service. sender sends after
// commit when the transaction has no email outbox.
func NewInvitationService(stores Stores, tx Transactor, sender email.Sender, frontendURL string) *InvitationService {
	return &InvitationService{stores: stores, tx: tx, sender: sender, frontendURL: frontendURL}
}

func invitationInvalid(detail string) error {
	return apperror.NewCode(identitycatalog.CodeInvitationInvalid, detail)
}

func membershipExists(detail string) error {
	return apperror.NewCode(identitycatalog.CodeMembershipExists, detail)
}

func invitationEvent(ctx context.Context, action string, inv *Invitation, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "invitation")
	event.TenantID = inv.TenantID
	event.ResourceID = strconv.FormatInt(inv.ID, 10)
	event.Metadata = metadata
	return event
}

// Invite creates (or refreshes) the pending invitation for an address and
// emails its link. An address that is already an active or banned member is
// refused. The invitation, its email job, and the invitation.created audit
// row commit together.
func (s *InvitationService) Invite(ctx context.Context, in InviteInput) (*Invitation, error) {
	addr := NormalizeEmail(in.Email)
	if !ValidRole(in.Role) {
		return nil, apperror.NewBadRequest("role must be one of: user, admin")
	}
	if err := s.ensureNotMember(ctx, addr); err != nil {
		return nil, err
	}
	token, tokenHash, err := newOpaqueToken()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate invitation token")
	}
	linkCtx, err := withLinkTenantSlug(ctx, s.stores.Tenants)
	if err != nil {
		return nil, apperror.NewInternal("failed to build invitation link")
	}
	msg := invitationMessage(addr, tokenLink(linkCtx, s.frontendURL, "accept-invitation", token))

	var (
		created *Invitation
		queued  bool
	)
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		inv, err := tx.Invitations.Upsert(ctx, NewInvitation{
			Email:     addr,
			Role:      in.Role,
			TokenHash: tokenHash,
			InvitedBy: in.InviterID,
			ExpiresAt: time.Now().Add(invitationTTL),
		})
		if err != nil {
			return apperror.NewInternal("failed to create invitation")
		}
		if tx.Mail != nil {
			if err := tx.Mail.Send(ctx, msg); err != nil {
				slog.Error("failed to enqueue invitation email", slog.Int64("invitation_id", inv.ID), slog.Any("error", err))
				return apperror.NewInternal("failed to create invitation")
			}
			queued = true
		}
		created = inv
		return checkAudit(tx.Audit.RecordTx(ctx, invitationEvent(ctx, "invitation.created", inv, map[string]any{"role": in.Role})))
	})
	if err != nil {
		return nil, err
	}
	if !queued {
		s.sendAsync(ctx, created.ID, msg)
	}
	return created, nil
}

// ensureNotMember refuses an address whose account is already an active or
// banned member of the request tenant, or that is banned there.
func (s *InvitationService) ensureNotMember(ctx context.Context, addr string) error {
	if err := ensureEmailNotBanned(ctx, s.stores.Memberships, addr); err != nil {
		return err
	}
	user, err := s.stores.Users.GetByEmail(ctx, addr)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return apperror.NewInternal("failed to check existing user")
	}
	member, err := s.stores.Memberships.Get(ctx, user.ID)
	switch {
	case errors.Is(err, apperror.ErrNotFound):
		return nil
	case err != nil:
		return apperror.NewInternal("failed to check membership")
	case member.Membership.Status == MembershipStatusRemoved:
		return nil
	default:
		return membershipExists("this address is already a member of the tenant")
	}
}

func invitationMessage(to, link string) email.Message {
	return email.Message{
		To:      []string{to},
		Subject: "You have been invited",
		HTML:    fmt.Sprintf("<p>You have been invited to join a workspace. Click <a href=%q>here</a> to accept. This link expires in 7 days.</p>", link),
	}
}

// sendAsync sends after commit when the sender cannot enqueue on the
// transaction; failures are logged. The request tenant is carried
// explicitly into the background send.
func (s *InvitationService) sendAsync(ctx context.Context, invitationID int64, msg email.Message) {
	if s.sender == nil {
		return
	}
	tenantID, tenantSlug := tenant.EffectiveID(ctx), tenant.SlugFromContext(ctx)
	async.Go(func() {
		sendCtx := tenant.WithIdentity(context.Background(), tenantID, tenantSlug)
		if err := s.sender.Send(sendCtx, msg); err != nil {
			slog.Warn("failed to send invitation email", slog.Int64("invitation_id", invitationID), slog.Any("error", err))
		}
	})
}

// List returns one cursor page of the request tenant's pending invitations.
func (s *InvitationService) List(ctx context.Context, limit int, startingAfter string) ([]Invitation, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	invitations, err := s.stores.Invitations.ListPendingCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list invitations")
	}
	hasMore := len(invitations) > pageSize
	if hasMore {
		invitations = invitations[:pageSize]
	}
	return invitations, hasMore, nil
}

// Revoke deletes a pending invitation of the request tenant.
func (s *InvitationService) Revoke(ctx context.Context, id int64) error {
	return s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		deleted, err := tx.Invitations.DeletePending(ctx, id)
		if err != nil {
			return apperror.NewInternal("failed to revoke invitation")
		}
		if !deleted {
			return apperror.NewNotFound("invitation not found")
		}
		inv := &Invitation{ID: id, TenantID: tenant.EffectiveID(ctx)}
		return checkAudit(tx.Audit.RecordTx(ctx, invitationEvent(ctx, "invitation.revoked", inv, nil)))
	})
}

// Accept adds an authenticated user to the invitation's tenant. The token is
// looked up by possession (the caller is signed in to another tenant); the
// invitation must be pending, unexpired, addressed to the caller's current
// email, and its tenant operational. Accepting proves control of the
// address, so it also marks the email verified. A removed membership is
// reactivated; an active or banned one is refused.
func (s *InvitationService) Accept(ctx context.Context, userID int64, token string) (*Member, error) {
	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		inv, tctx, err := s.pendingInvitation(ctx, tx.Stores, token)
		if err != nil {
			return err
		}
		user, err := tx.Users.GetByID(ctx, userID)
		if err != nil || user.Status != UserStatusActive {
			return accountNotActive()
		}
		if user.Email != inv.Email {
			return invitationInvalid("this invitation was sent to a different email address")
		}
		joined, err := joinTenant(tctx, tx.Stores, user, inv)
		if err != nil {
			return err
		}
		if err := markAccepted(ctx, tx.Invitations, inv, userID); err != nil {
			return err
		}
		member = joined
		event := invitationEvent(tctx, "invitation.accepted", inv, map[string]any{"signup": false, "role": inv.Role})
		event.ActorID, event.ActorType = userID, audit.ActorTypeUser
		return checkAudit(tx.Audit.RecordTx(tctx, event))
	})
	return member, err
}

// SignUp creates an account from an invitation (the invitee has no account
// yet). The request tenant must be the invitation's tenant (the emailed link
// carries its slug). The account's email is verified (the token proves
// control of the address), and the user, password, membership, and audit row
// commit together. It is not gated by user_registration: the tenant admin
// invited this address explicitly.
func (s *InvitationService) SignUp(ctx context.Context, in InvitationSignUp) (*Member, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}
	var member *Member
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		inv, tctx, err := s.pendingInvitation(ctx, tx.Stores, in.Token)
		if err != nil {
			return err
		}
		if inv.TenantID != tenant.EffectiveID(ctx) {
			return invitationInvalid("invalid or expired invitation")
		}
		if _, err := tx.Users.GetByEmail(ctx, inv.Email); err == nil {
			return apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered; sign in and accept the invitation")
		} else if !errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewInternal("failed to check existing user")
		}
		created, err := createAccount(tctx, tx.Stores, NewUser{
			Email: inv.Email, Name: strings.TrimSpace(in.Name), EmailVerified: true,
		}, string(hash), inv.Role)
		if err != nil {
			return err
		}
		if err := markAccepted(ctx, tx.Invitations, inv, created.User.ID); err != nil {
			return err
		}
		member = created
		event := invitationEvent(tctx, "invitation.accepted", inv, map[string]any{"signup": true, "role": inv.Role})
		event.ActorID, event.ActorType = created.User.ID, audit.ActorTypeUser
		return checkAudit(tx.Audit.RecordTx(tctx, event))
	})
	return member, err
}

// markAccepted stamps the (row-locked) invitation as accepted by userID. A
// zero-row update means it is no longer pending, which the lock makes
// impossible; it still fails closed rather than committing a membership
// without its accepted invitation.
func markAccepted(ctx context.Context, invitations InvitationRepository, inv *Invitation, userID int64) error {
	accepted, err := invitations.MarkAccepted(ctx, inv.TenantID, inv.ID, userID)
	switch {
	case err != nil:
		return apperror.NewInternal("failed to accept invitation")
	case !accepted:
		return invitationInvalid("invalid or expired invitation")
	default:
		return nil
	}
}

// pendingInvitation locks the invitation of token and returns it with a
// context scoped to its tenant.
func (s *InvitationService) pendingInvitation(ctx context.Context, st Stores, token string) (*Invitation, context.Context, error) {
	const invalid = "invalid or expired invitation"
	inv, err := st.Invitations.GetByTokenHashForUpdate(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, invitationInvalid(invalid)
		}
		return nil, nil, apperror.NewInternal("failed to load invitation")
	}
	if inv.AcceptedAt != nil || !inv.ExpiresAt.After(time.Now()) {
		return nil, nil, invitationInvalid(invalid)
	}
	if _, err := st.Tenants.GetOperationalByID(ctx, inv.TenantID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, invitationInvalid(invalid)
		}
		return nil, nil, apperror.NewInternal("failed to load tenant")
	}
	tctx := tenant.WithID(ctx, inv.TenantID)
	if err := ensureEmailNotBanned(tctx, st.Memberships, inv.Email); err != nil {
		return nil, nil, err
	}
	return inv, tctx, nil
}

// joinTenant creates or reactivates user's membership in tctx's tenant. It
// reads first instead of relying on the unique violation, which would abort
// the surrounding transaction; the invitation row lock serializes accepts of
// this invitation, and a concurrent join through another path still fails
// closed on uq_memberships_tenant_user.
func joinTenant(tctx context.Context, st Stores, user *User, inv *Invitation) (*Member, error) {
	var membership *Membership
	existing, err := st.Memberships.Get(tctx, user.ID)
	switch {
	case errors.Is(err, apperror.ErrNotFound):
		membership, err = st.Memberships.Create(tctx, user.ID, inv.Role)
	case err != nil:
		return nil, apperror.NewInternal("failed to check membership")
	case existing.Membership.Status == MembershipStatusRemoved:
		membership, err = st.Memberships.Reactivate(tctx, user.ID, inv.Role)
	default:
		// An active member cannot re-join, and a ban is never lifted here.
		return nil, membershipExists("you are already a member of this tenant")
	}
	if err != nil {
		if errors.Is(err, ErrAlreadyMember) {
			return nil, membershipExists("you are already a member of this tenant")
		}
		return nil, apperror.NewInternal("failed to join tenant")
	}
	verified, err := st.Users.VerifyEmail(tctx, user.ID, inv.Email)
	if err != nil {
		return nil, apperror.NewInternal("failed to join tenant")
	}
	return &Member{User: *verified, Membership: *membership}, nil
}
