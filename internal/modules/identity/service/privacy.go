package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const (
	// JobKindDataExport builds one export document.
	JobKindDataExport = "privacy.data_export"
	// JobKindErase erases one user's data in one tenant.
	JobKindErase = "privacy.erase"
	// JobKindEraseFinalize anonymizes the global user once every membership is
	// erased.
	JobKindEraseFinalize = "privacy.erase.finalize"
	// JobKindExportPurge removes expired export blobs and rows.
	JobKindExportPurge = "privacy.exports.purge"

	// DataExportTTL is how long a ready export stays downloadable.
	DataExportTTL = 7 * 24 * time.Hour
	// ExportPurgeInterval is the hourly retention cadence.
	ExportPurgeInterval = time.Hour

	exportPurgeBatchSize = 100
	exportPurgeBudget    = time.Minute

	// identityProviderName is identity's own privacy provider. Its eraser
	// removes the membership, so it runs last in a tenant erase job.
	identityProviderName = "identity"
)

var (
	errErasurePending = errors.New("private erasure is waiting for tenant memberships")
	// errErasureNotRequested refuses a privacy.erase job whose membership was
	// never reserved by a request transaction. A job-time refusal is
	// permanent: retrying cannot make a request that never happened.
	errErasureNotRequested = errors.New("erasure was not requested for this membership")
	// errErasureLastOwner refuses a job whose member became the last active
	// owner after the request reserved it.
	errErasureLastOwner = errors.New("erasing this member would remove the last active owner")
)

type exportJob struct {
	ExportID int64 `json:"exportId"`
	UserID   int64 `json:"userId"`
}

type eraseJob struct {
	UserID int64 `json:"userId"`
}

type finalizeJob struct {
	UserID int64 `json:"userId"`
}

type exportDocument struct {
	UserID      int64                      `json:"userId"`
	TenantID    int64                      `json:"tenantId"`
	GeneratedAt time.Time                  `json:"generatedAt"`
	Sections    map[string]json.RawMessage `json:"sections"`
}

type identityExport struct {
	Profile struct {
		ID            int64      `json:"id"`
		Email         string     `json:"email"`
		Name          string     `json:"name"`
		EmailVerified bool       `json:"emailVerified"`
		Status        string     `json:"status"`
		CreatedAt     time.Time  `json:"createdAt"`
		UpdatedAt     time.Time  `json:"updatedAt"`
		DeletedAt     *time.Time `json:"deletedAt,omitempty"`
	} `json:"profile"`
	Membership struct {
		TenantID  int64      `json:"tenantId"`
		Role      string     `json:"role"`
		Status    string     `json:"status"`
		JoinedAt  time.Time  `json:"joinedAt"`
		BannedAt  *time.Time `json:"bannedAt,omitempty"`
		RemovedAt *time.Time `json:"removedAt,omitempty"`
	} `json:"membership"`
	Sessions []sessionExport `json:"sessions"`
	// Identities are linked external accounts (provider, subject, linked_at);
	// no token is ever stored or exported for them.
	Identities []externalIdentityExport `json:"identities"`
	// MFA reports configuration state only: never the sealed TOTP secret or a
	// recovery-code hash.
	MFA mfaExport `json:"mfa"`
	// RoleAssignments are the current membership roles in this tenant. Role
	// changes are not historized, so this is the full role history the data
	// model keeps.
	RoleAssignments []roleAssignmentExport `json:"roleAssignments"`
	// InvitationsSent / InvitationsReceived are the user's invitations in this
	// tenant (created by them, or addressed to them / accepted by them).
	InvitationsSent     []invitationExport `json:"invitationsSent"`
	InvitationsReceived []invitationExport `json:"invitationsReceived"`
	// AuditEvents is capped at privacyAuditEventLimit, newest first. Only the
	// user's own rows are included; audit metadata is never exported.
	AuditEvents []auditEventExport `json:"auditEvents"`
}

type externalIdentityExport struct {
	Provider string    `json:"provider"`
	Subject  string    `json:"subject"`
	LinkedAt time.Time `json:"linkedAt"`
}

type mfaExport struct {
	Enabled                bool       `json:"enabled"`
	ConfirmedAt            *time.Time `json:"confirmedAt,omitempty"`
	RecoveryCodesRemaining int64      `json:"recoveryCodesRemaining"`
}

type roleAssignmentExport struct {
	Key        string    `json:"key"`
	AssignedAt time.Time `json:"assignedAt"`
}

type invitationExport struct {
	ID         int64      `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	InvitedBy  int64      `json:"invitedBy"`
	AcceptedBy *int64     `json:"acceptedBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
}

type auditEventExport struct {
	Action     string    `json:"action"`
	CreatedAt  time.Time `json:"createdAt"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	Resource   string    `json:"resource"`
	ResourceID string    `json:"resourceId,omitempty"`
	Outcome    string    `json:"outcome"`
}

const (
	// privacyAuditEventLimit bounds the user's own audit rows in one export so
	// a long-lived account cannot make the document unbounded.
	privacyAuditEventLimit = 1000
)

// IdentityExportRepository is the identity-only read side of the export:
// linked external identities, current role assignments, and invitations.
type IdentityExportRepository interface {
	ListExternalIdentities(ctx context.Context, userID int64) ([]ExternalIdentityExport, error)
	ListRoleAssignments(ctx context.Context, tenantID, userID int64) ([]RoleAssignmentExport, error)
	ListInvitationsSent(ctx context.Context, tenantID, userID int64) ([]InvitationExport, error)
	ListInvitationsReceived(ctx context.Context, tenantID, userID int64, email string) ([]InvitationExport, error)
}

// ExternalIdentityExport is one linked provider account without any token.
type ExternalIdentityExport struct {
	Provider string
	Subject  string
	LinkedAt time.Time
}

// RoleAssignmentExport is one current membership role assignment.
type RoleAssignmentExport struct {
	Key        string
	AssignedAt time.Time
}

// InvitationExport is one invitation the user sent or received.
type InvitationExport struct {
	ID         int64
	Email      string
	Role       string
	InvitedBy  int64
	AcceptedBy *int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
}

type sessionExport struct {
	ID            int64      `json:"id"`
	UserAgent     string     `json:"userAgent,omitempty"`
	IP            string     `json:"ip,omitempty"`
	LastUsedAt    time.Time  `json:"lastUsedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
	RevokedReason string     `json:"revokedReason,omitempty"`
}

// PasswordVerifier re-authenticates a self-service erasure request.
type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, userID int64, password string) error
}

// PrivacyDeps are the privacy service's collaborators. Registry and Notifier
// are populated late (registry by app.PrivacyProvider, notifier by
// identity.Module.SetNotifier).
type PrivacyDeps struct {
	Stores       Stores
	Tx           Transactor
	Storage      storage.Storage
	Registry     *privacycontracts.Registry
	Notifier     notificationcontracts.Notifier
	Verifier     PasswordVerifier
	SessionCache session.Store
	// ExportStore supplies the identity-only export reads (external
	// identities, roles, invitations). Nil in fakes that do not exercise them.
	ExportStore IdentityExportRepository
	// AuditEvents reads the user's own audit rows; nil exports an empty list.
	AuditEvents audit.UserEventReader
}

// PrivacyService owns data exports, the erasure fan-out, and the identity
// module's own exporter/eraser.
type PrivacyService struct {
	stores       Stores
	tx           Transactor
	storage      storage.Storage
	registry     *privacycontracts.Registry
	verifier     PasswordVerifier
	sessionCache session.Store
	exportStore  IdentityExportRepository
	auditEvents  audit.UserEventReader

	mu       sync.RWMutex
	notifier notificationcontracts.Notifier
}

// NewPrivacyService wires the service. It may be built before the registry and
// notifier are populated.
func NewPrivacyService(deps PrivacyDeps) *PrivacyService {
	return &PrivacyService{
		stores:       deps.Stores,
		tx:           deps.Tx,
		storage:      deps.Storage,
		registry:     deps.Registry,
		verifier:     deps.Verifier,
		sessionCache: deps.SessionCache,
		exportStore:  deps.ExportStore,
		auditEvents:  deps.AuditEvents,
		notifier:     deps.Notifier,
	}
}

// SetNotifier wires the notifications contract after the notifications module
// is built.
func (s *PrivacyService) SetNotifier(notifier notificationcontracts.Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = notifier
}

func (s *PrivacyService) currentNotifier() notificationcontracts.Notifier {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.notifier
}

// RequestExport creates a tenant-scoped export row and enqueues its build job.
func (s *PrivacyService) RequestExport(ctx context.Context, userID int64) (*DataExport, error) {
	var export *DataExport
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if tx.Privacy == nil || tx.Jobs == nil {
			return apperror.NewInternal("privacy services are not configured")
		}
		// Hold the active membership FOR SHARE until the row commits: an
		// erasure that removes it either finishes first (404 here) or waits and
		// then sees and deletes this export.
		if _, err := tx.Memberships.LockPrincipal(ctx, userID); err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to load membership")
		}
		created, err := tx.Privacy.Create(ctx, userID)
		if err != nil {
			if errors.Is(err, ErrDataExportInProgress) {
				return apperror.NewCode(identitycatalog.CodeDataExportInProgress, "a data export is already in progress")
			}
			return apperror.NewInternal("failed to create data export")
		}
		export = created
		payload, _ := json.Marshal(exportJob{ExportID: created.ID, UserID: userID})
		if _, err := tx.Jobs.Enqueue(ctx, jobs.Job{
			Kind: JobKindDataExport, Payload: payload, MaxAttempts: 5,
		}); err != nil {
			return apperror.NewInternal("failed to queue data export")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return export, nil
}

// ListExports returns one cursor page of the caller's exports, newest first.
func (s *PrivacyService) ListExports(ctx context.Context, userID int64, limit int, startingAfter string) ([]DataExport, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	exports, err := s.stores.Privacy.ListCursor(ctx, userID, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list data exports")
	}
	hasMore := len(exports) > pageSize
	if hasMore {
		exports = exports[:pageSize]
	}
	return exports, hasMore, nil
}

// GetExport returns one of the caller's exports. Another user's or tenant's id
// is not found.
func (s *PrivacyService) GetExport(ctx context.Context, userID, id int64) (*DataExport, error) {
	export, err := s.stores.Privacy.Get(ctx, userID, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewCode(identitycatalog.CodeDataExportNotFound, "data export not found")
		}
		return nil, apperror.NewInternal("failed to get data export")
	}
	return export, nil
}

// DownloadExport opens the caller's ready export.
func (s *PrivacyService) DownloadExport(ctx context.Context, userID, id int64) (*DataExport, io.ReadCloser, error) {
	export, err := s.GetExport(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	if export.Status == DataExportExpired || (export.ExpiresAt != nil && !export.ExpiresAt.After(time.Now())) {
		return nil, nil, apperror.NewCode(identitycatalog.CodeDataExportExpired, "data export has expired")
	}
	if export.Status != DataExportReady || export.StoragePath == "" {
		return nil, nil, apperror.NewCode(identitycatalog.CodeDataExportNotReady, "data export is not ready")
	}
	reader, err := s.storage.Get(context.WithoutCancel(ctx), export.StoragePath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			slog.Error("data export row exists but its document is missing",
				slog.Int64("export_id", export.ID), slog.String("path", export.StoragePath))
			return nil, nil, apperror.NewCode(identitycatalog.CodeDataExportNotFound, "data export not found")
		}
		return nil, nil, apperror.NewInternal("failed to read data export")
	}
	return export, reader, nil
}

// RequestSelfErasure re-authenticates the caller and starts global erasure.
// The request transaction reserves every membership (suspends it under the
// tenant owner lock and revokes its sessions), so the account cannot act while
// the erase jobs are pending.
func (s *PrivacyService) RequestSelfErasure(ctx context.Context, userID int64, password string) error {
	if s.verifier == nil {
		return apperror.NewInternal("password verification is not configured")
	}
	if err := s.verifier.VerifyPassword(ctx, userID, password); err != nil {
		return err
	}
	if err := s.requestErasure(ctx, userID, "user.erasure_requested"); err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessionCache, userID)
	return nil
}

// RequestAdminErasure starts erasure for a member of the request tenant. It
// is deliberately tenant-scoped: the request tenant's membership is removed
// and only that tenant's data is erased, while the global account and the
// user's memberships in other tenants stay intact. The membership is
// suspended inside the request transaction, so a second request (including a
// concurrent one) sees the reservation and acts on the remaining owners.
func (s *PrivacyService) RequestAdminErasure(ctx context.Context, userID int64) error {
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if tx.Privacy == nil || tx.Jobs == nil {
			return apperror.NewInternal("privacy services are not configured")
		}
		if err := EnsureCanManageMember(ctx, tx.Stores, userID); err != nil {
			return err
		}
		member, err := tx.Memberships.Get(ctx, userID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to get user")
		}
		// A cross-tenant id and a member who already left both look absent.
		if member.Membership.Status == MembershipStatusRemoved {
			return apperror.NewNotFound("user not found")
		}
		if member.Membership.Status == MembershipStatusActive && member.Membership.Role == RoleOwner {
			if err := ensureNotLastOwner(ctx, tx.Memberships, "cannot erase the last active owner"); err != nil {
				return err
			}
		}
		if err := tx.Memberships.SuspendForErasure(ctx, userID); err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to reserve the member for erasure")
		}
		if err := tx.Sessions.RevokeAllInTenant(ctx, userID, RevokeMembershipChanged); err != nil {
			return apperror.NewInternal("failed to revoke sessions")
		}
		if _, err := tx.Privacy.ExpireForUser(ctx, userID); err != nil {
			return apperror.NewInternal("failed to expire data exports")
		}
		erasePayload, _ := json.Marshal(eraseJob{UserID: userID})
		if _, err := tx.Jobs.Enqueue(ctx, jobs.Job{
			TenantID: tenant.EffectiveID(ctx), Kind: JobKindErase, Payload: erasePayload, MaxAttempts: 20,
		}); err != nil {
			return apperror.NewInternal("failed to queue erasure")
		}
		event := userEvent(ctx, "admin.user.erasure_requested", userID, map[string]any{"scope": "tenant"})
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessionCache, userID)
	return nil
}

// requestErasure refuses to erase the last active owner of any tenant, then
// reserves every membership (suspended, sessions revoked) and commits one
// erase job per membership plus the global finalizer and a Tier 1 audit row.
// Reserving inside the request transaction is what makes a second owner's
// request see one active owner and fail up front; the job then only proceeds
// for a membership marked for erasure.
func (s *PrivacyService) requestErasure(ctx context.Context, userID int64, action string) error {
	ctx = database.WithRLSBypass(ctx) // Erasure reads and writes every tenant the user belongs to.
	return s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if tx.Privacy == nil || tx.Jobs == nil {
			return apperror.NewInternal("privacy services are not configured")
		}
		memberships, err := tx.Memberships.ListForUser(ctx, userID)
		if err != nil {
			return apperror.NewInternal("failed to load memberships")
		}
		if len(memberships) == 0 {
			return apperror.NewNotFound("user not found")
		}
		sort.Slice(memberships, func(i, j int) bool { return memberships[i].TenantID < memberships[j].TenantID })

		erasePayload, _ := json.Marshal(eraseJob{UserID: userID})
		for i := range memberships {
			m := &memberships[i]
			tctx := tenant.WithID(ctx, m.TenantID)
			queue, err := reserveMembershipForErasure(tctx, tx, userID)
			if err != nil {
				return err
			}
			if !queue {
				continue
			}
			if _, err := tx.Privacy.ExpireForUser(tctx, userID); err != nil {
				return apperror.NewInternal("failed to expire data exports")
			}
			if _, err := tx.Jobs.Enqueue(tctx, jobs.Job{
				TenantID: m.TenantID, Kind: JobKindErase, Payload: erasePayload, MaxAttempts: 20,
			}); err != nil {
				return apperror.NewInternal("failed to queue erasure")
			}
		}

		finalizePayload, _ := json.Marshal(finalizeJob{UserID: userID})
		if _, err := tx.Jobs.Enqueue(ctx, jobs.Job{
			Kind: JobKindEraseFinalize, Payload: finalizePayload, MaxAttempts: 20,
		}); err != nil {
			return apperror.NewInternal("failed to queue erasure finalization")
		}

		event := userEvent(ctx, action, userID, map[string]any{"memberships": len(memberships)})
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
}

// reserveMembershipForErasure suspends one membership inside the request
// transaction, without ban markers, and revokes its sessions. It reports
// queue=false when the membership is already reserved by an earlier request
// (its job exists) and true when the caller must enqueue one. An already
// removed membership has no row to reserve but still needs its erasers run.
// Lock order matches every membership mutation: the target row first, then the
// per-tenant owner-change lock inside ensureNotLastOwner.
func reserveMembershipForErasure(ctx context.Context, tx TxRepos, userID int64) (bool, error) {
	if err := tx.Memberships.LockMembership(ctx, userID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return false, nil
		}
		return false, apperror.NewInternal("failed to lock membership")
	}
	current, err := tx.Memberships.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return false, nil
		}
		return false, apperror.NewInternal("failed to load membership")
	}
	if current.Membership.Status == MembershipStatusRemoved {
		return true, nil
	}
	if current.Membership.ErasureRequestedAt != nil {
		return false, nil
	}
	if current.Membership.Status == MembershipStatusActive && current.Membership.Role == RoleOwner {
		if err := ensureNotLastOwner(ctx, tx.Memberships, "cannot erase the last active owner"); err != nil {
			return false, err
		}
	}
	if err := tx.Memberships.SuspendForErasure(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to reserve the member for erasure")
	}
	if err := tx.Sessions.RevokeAllInTenant(ctx, userID, RevokeMembershipChanged); err != nil {
		return false, apperror.NewInternal("failed to revoke sessions")
	}
	return true, nil
}

// Export implements privacy.Exporter for identity's own data.
func (s *PrivacyService) Export(ctx context.Context, tenantID, userID int64) (privacycontracts.Section, error) {
	tctx := tenant.WithID(ctx, tenantID)
	user, err := s.stores.Users.GetByID(tctx, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	member, err := s.stores.Memberships.Get(tctx, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	sessions, err := s.stores.Privacy.ListSessions(tctx, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}

	var out identityExport
	out.Profile.ID = user.ID
	out.Profile.Email = user.Email
	out.Profile.Name = user.Name
	out.Profile.EmailVerified = user.EmailVerified()
	out.Profile.Status = string(user.Status)
	out.Profile.CreatedAt = user.CreatedAt
	out.Profile.UpdatedAt = user.UpdatedAt
	out.Profile.DeletedAt = user.DeletedAt
	out.Membership.TenantID = member.Membership.TenantID
	out.Membership.Role = member.Membership.Role
	out.Membership.Status = string(member.Membership.Status)
	out.Membership.JoinedAt = member.Membership.CreatedAt
	out.Membership.BannedAt = member.Membership.BannedAt
	out.Membership.RemovedAt = member.Membership.RemovedAt
	out.Sessions = make([]sessionExport, len(sessions))
	for i, sess := range sessions {
		out.Sessions[i] = sessionExport{
			ID: sess.ID, UserAgent: sess.UserAgent, IP: sess.IP,
			LastUsedAt: sess.LastUsedAt, CreatedAt: sess.CreatedAt,
			RevokedAt: sess.RevokedAt, RevokedReason: string(sess.RevokedReason),
		}
	}
	if s.exportStore != nil {
		identities, err := s.exportStore.ListExternalIdentities(tctx, userID)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		out.Identities = make([]externalIdentityExport, len(identities))
		for i, identity := range identities {
			out.Identities[i] = externalIdentityExport(identity)
		}
		roles, err := s.exportStore.ListRoleAssignments(tctx, tenantID, userID)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		out.RoleAssignments = make([]roleAssignmentExport, len(roles))
		for i, role := range roles {
			out.RoleAssignments[i] = roleAssignmentExport(role)
		}
		sent, err := s.exportStore.ListInvitationsSent(tctx, tenantID, userID)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		out.InvitationsSent = toInvitationExports(sent)
		received, err := s.exportStore.ListInvitationsReceived(tctx, tenantID, userID, user.Email)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		out.InvitationsReceived = toInvitationExports(received)
	}
	if out.Identities == nil {
		out.Identities = []externalIdentityExport{}
	}
	if out.RoleAssignments == nil {
		out.RoleAssignments = []roleAssignmentExport{}
	}
	if out.InvitationsSent == nil {
		out.InvitationsSent = []invitationExport{}
	}
	if out.InvitationsReceived == nil {
		out.InvitationsReceived = []invitationExport{}
	}

	mfa, err := s.exportMFAStatus(tctx, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	out.MFA = mfa

	if s.auditEvents != nil {
		events, err := s.auditEvents.ListUserEvents(tctx, tenantID, userID, privacyAuditEventLimit)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		out.AuditEvents = make([]auditEventExport, len(events))
		for i, event := range events {
			out.AuditEvents[i] = auditEventExport{
				Action: event.Action, CreatedAt: event.CreatedAt,
				IP: event.IP, UserAgent: event.UserAgent,
				Resource: event.Resource, ResourceID: event.ResourceID, Outcome: event.Outcome,
			}
		}
	}
	if out.AuditEvents == nil {
		out.AuditEvents = []auditEventExport{}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, fmt.Errorf("marshal identity export: %w", err)
	}
	return privacycontracts.Section{Name: "identity", Data: data}, nil
}

// exportMFAStatus reports configuration only: the sealed secret and the
// recovery-code hashes never enter the document.
func (s *PrivacyService) exportMFAStatus(ctx context.Context, userID int64) (mfaExport, error) {
	if s.stores.MFA == nil {
		return mfaExport{}, nil
	}
	factor, err := s.stores.MFA.GetTOTP(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return mfaExport{}, nil
		}
		return mfaExport{}, err
	}
	remaining, err := s.stores.MFA.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return mfaExport{}, err
	}
	return mfaExport{
		Enabled:                factor.Confirmed(),
		ConfirmedAt:            factor.ConfirmedAt,
		RecoveryCodesRemaining: remaining,
	}, nil
}

func toInvitationExports(invitations []InvitationExport) []invitationExport {
	now := time.Now()
	out := make([]invitationExport, len(invitations))
	for i, invitation := range invitations {
		status := "pending"
		switch {
		case invitation.AcceptedAt != nil:
			status = "accepted"
		case !invitation.ExpiresAt.After(now):
			status = "expired"
		}
		out[i] = invitationExport{
			ID: invitation.ID, Email: invitation.Email, Role: invitation.Role, Status: status,
			InvitedBy: invitation.InvitedBy, AcceptedBy: invitation.AcceptedBy,
			CreatedAt: invitation.CreatedAt, ExpiresAt: invitation.ExpiresAt,
			AcceptedAt: invitation.AcceptedAt,
		}
	}
	return out
}

// Erase implements privacy.Eraser for identity's own data: export documents,
// invitation addresses, sessions, and the tenant membership.
func (s *PrivacyService) Erase(ctx context.Context, tenantID, userID int64) error {
	tctx := tenant.WithID(ctx, tenantID)
	if err := s.eraseExports(tctx, userID); err != nil {
		return err
	}
	if _, err := s.stores.Privacy.AnonymizeErasedUserInvitations(tctx, userID, erasedEmail(userID)); err != nil {
		return fmt.Errorf("anonymize invitations: %w", err)
	}
	if err := s.stores.Privacy.DeleteSessionsInTenant(tctx, userID); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}

	member, err := s.stores.Memberships.Get(tctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get membership: %w", err)
	}
	if member.Membership.Status == MembershipStatusRemoved {
		return nil
	}
	if _, err := s.stores.Memberships.Remove(tctx, userID); err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return fmt.Errorf("remove membership: %w", err)
	}
	return nil
}

// eraseExports expires every export, deletes each blob (a missing blob counts
// as deleted), then deletes the rows. Already-expired rows are included, so an
// erasure request that pre-expired them cannot strand their documents.
func (s *PrivacyService) eraseExports(ctx context.Context, userID int64) error {
	if _, err := s.stores.Privacy.ExpireForUser(ctx, userID); err != nil {
		return fmt.Errorf("expire data exports: %w", err)
	}
	exports, err := s.stores.Privacy.ListForUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list data exports: %w", err)
	}
	for _, export := range exports {
		if export.StoragePath == "" {
			continue
		}
		if err := s.storage.Delete(ctx, export.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete data export %d: %w", export.ID, err)
		}
	}
	if err := s.stores.Privacy.DeleteForUser(ctx, userID); err != nil {
		return fmt.Errorf("delete data exports: %w", err)
	}
	return nil
}

// HandleExportJob builds the document, stores it, marks the row ready, and
// notifies the user. At-least-once delivery makes it idempotent.
func (s *PrivacyService) HandleExportJob(ctx context.Context, payload json.RawMessage) error {
	var in exportJob
	if err := json.Unmarshal(payload, &in); err != nil || in.ExportID <= 0 || in.UserID <= 0 {
		return jobs.Permanent(fmt.Errorf("invalid privacy.data_export payload"))
	}
	export, err := s.stores.Privacy.Get(ctx, in.UserID, in.ExportID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("load data export: %w", err)
	}
	if export.Status == DataExportReady || export.Status == DataExportExpired {
		return nil
	}
	running, err := s.stores.Privacy.MarkRunning(ctx, in.UserID, in.ExportID)
	if err != nil {
		return fmt.Errorf("claim data export: %w", err)
	}
	if !running {
		return nil
	}
	finished := false
	defer func() {
		if !finished {
			s.markExportFailed(ctx, in.UserID, in.ExportID)
		}
	}()

	document, err := s.buildExportDocument(ctx, export)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%d/%d/exports/%s.json", export.TenantID, export.UserID, uuid.NewString())
	if err := s.storage.Put(ctx, path, bytes.NewReader(document), int64(len(document)), "application/json"); err != nil {
		return fmt.Errorf("store data export: %w", err)
	}
	expires := time.Now().Add(DataExportTTL)
	ready, err := s.stores.Privacy.MarkReady(ctx, in.UserID, in.ExportID, path, expires)
	if err != nil {
		_ = s.storage.Delete(context.WithoutCancel(ctx), path)
		return fmt.Errorf("mark data export ready: %w", err)
	}
	if !ready {
		_ = s.storage.Delete(context.WithoutCancel(ctx), path)
		return nil
	}
	finished = true

	if notifier := s.currentNotifier(); notifier != nil {
		data, _ := json.Marshal(map[string]int64{"exportId": in.ExportID})
		if err := notifier.Notify(ctx, notificationcontracts.CreateNotificationInput{
			TenantID: export.TenantID,
			UserID:   export.UserID,
			Type:     "privacy.data_export_ready",
			Title:    "Your data export is ready",
			Body:     "Download it from your account within 7 days.",
			Data:     data,
		}); err != nil {
			slog.Warn("failed to notify user about a ready data export",
				slog.Int64("export_id", in.ExportID), slog.Any("error", err))
		}
	}
	return nil
}

// markExportFailed records a terminal failure even when the job context was
// canceled or a provider panicked. MarkFailed is a no-op for ready/expired
// rows, so the cleanup is safe after a late erasure.
func (s *PrivacyService) markExportFailed(ctx context.Context, userID, exportID int64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.stores.Privacy.MarkFailed(cleanupCtx, userID, exportID); err != nil {
		slog.Warn("failed to mark data export failed",
			slog.Int64("export_id", exportID), slog.Any("error", err))
	}
}

func (s *PrivacyService) buildExportDocument(ctx context.Context, export *DataExport) ([]byte, error) {
	doc := exportDocument{
		UserID:      export.UserID,
		TenantID:    export.TenantID,
		GeneratedAt: time.Now().UTC(),
		Sections:    map[string]json.RawMessage{},
	}
	if s.registry != nil {
		for _, provider := range s.registry.Providers() {
			section, err := provider.Exporter.Export(ctx, export.TenantID, export.UserID)
			if err != nil {
				return nil, fmt.Errorf("export %s: %w", provider.Name, err)
			}
			if len(section.Data) == 0 || !json.Valid(section.Data) {
				return nil, fmt.Errorf("export %s: section is not valid JSON", provider.Name)
			}
			doc.Sections[provider.Name] = section.Data
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal data export: %w", err)
	}
	return raw, nil
}

// HandleEraseJob runs every tenant Eraser, identity's own eraser last: it
// removes the membership, and the finalizer treats a removed membership as
// proof that every other module's data is gone. Each implementation is
// idempotent, so a retry is safe. The request transaction already reserved the
// membership (suspended, sessions revoked); a job for a live membership
// without that reservation is refused permanently and logged with both ids.
func (s *PrivacyService) HandleEraseJob(ctx context.Context, payload json.RawMessage) error {
	var in eraseJob
	if err := json.Unmarshal(payload, &in); err != nil || in.UserID <= 0 {
		return jobs.Permanent(fmt.Errorf("invalid privacy.erase payload"))
	}
	tenantID := tenant.EffectiveID(ctx)
	if err := s.checkErasureRequest(ctx, in.UserID); err != nil {
		if errors.Is(err, errErasureNotRequested) || errors.Is(err, errErasureLastOwner) {
			slog.Error("privacy.erase job refused",
				slog.Int64("tenant_id", tenantID), slog.Int64("user_id", in.UserID), slog.Any("error", err))
			return jobs.Permanent(fmt.Errorf("refusing to erase user %d in tenant %d: %w", in.UserID, tenantID, err))
		}
		return err
	}
	return s.eraseUserInTenant(ctx, tenantID, in.UserID)
}

// eraseUserInTenant runs every registered tenant eraser for one user, identity
// last: its eraser removes the membership, so a removed membership proves the
// other modules finished. The tenant purge reuses it per membership.
func (s *PrivacyService) eraseUserInTenant(ctx context.Context, tenantID, userID int64) error {
	var providers []privacycontracts.Provider
	if s.registry != nil {
		providers = s.registry.Providers()
	}
	identitySeen := false
	for _, provider := range providers {
		if provider.Name == identityProviderName {
			continue
		}
		if err := provider.Eraser.Erase(ctx, tenantID, userID); err != nil {
			return fmt.Errorf("erase %s: %w", provider.Name, err)
		}
	}
	for _, provider := range providers {
		if provider.Name != identityProviderName {
			continue
		}
		identitySeen = true
		if err := provider.Eraser.Erase(ctx, tenantID, userID); err != nil {
			return fmt.Errorf("erase %s: %w", provider.Name, err)
		}
	}
	if !identitySeen {
		// Without identity's eraser the membership stays and no finalizer can
		// make progress; fail loudly instead of reporting a partial erase.
		return fmt.Errorf("identity privacy eraser is not registered")
	}
	return nil
}

// checkErasureRequest gates a job on the request-time reservation. A removed
// membership (or one already hard-deleted) is a completed no-op; a live one
// without erasure_requested_at was never requested. The last-owner check
// remains a defense in case the membership was reactivated after reserving.
func (s *PrivacyService) checkErasureRequest(ctx context.Context, userID int64) error {
	return s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		member, err := tx.Memberships.Get(ctx, userID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("load membership: %w", err)
		}
		if member.Membership.Status == MembershipStatusRemoved {
			return nil
		}
		if member.Membership.ErasureRequestedAt == nil {
			return errErasureNotRequested
		}
		if member.Membership.Status == MembershipStatusActive && member.Membership.Role == RoleOwner {
			if err := ensureNotLastOwner(ctx, tx.Memberships, "cannot erase the last active owner"); err != nil {
				var appErr *apperror.AppError
				if errors.As(err, &appErr) && appErr.Code == string(apperror.CodeUserLastAdmin) {
					return errErasureLastOwner
				}
				return err
			}
		}
		return nil
	})
}

// HandleFinalizeJob anonymizes the global user only after every membership has
// been removed. Until then it fails so the job retries with backoff.
func (s *PrivacyService) HandleFinalizeJob(ctx context.Context, payload json.RawMessage) error {
	var in finalizeJob
	if err := json.Unmarshal(payload, &in); err != nil || in.UserID <= 0 {
		return jobs.Permanent(fmt.Errorf("invalid privacy.erase.finalize payload"))
	}
	anonymized, err := s.finalizeUser(ctx, in.UserID)
	if err != nil {
		return err
	}
	if anonymized {
		invalidateSessions(ctx, s.sessionCache, in.UserID)
	}
	return nil
}

func (s *PrivacyService) finalizeUser(ctx context.Context, userID int64) (bool, error) {
	ctx = database.WithRLSBypass(ctx) // The finalizer checks and cleans every tenant.
	memberships, err := s.stores.Memberships.ListForUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("load memberships: %w", err)
	}
	if !allMembershipsRemoved(memberships) {
		return false, errErasurePending
	}
	// Sweep exports created just before the last membership was removed, and
	// every invitation address in every tenant (including one the user never
	// joined). The request path holds the membership FOR SHARE, so no new
	// export can appear after this point.
	replacement := erasedEmail(userID)
	for i := range memberships {
		tctx := tenant.WithID(ctx, memberships[i].TenantID)
		if err := s.eraseExports(tctx, userID); err != nil {
			return false, err
		}
	}
	if _, err := s.stores.Privacy.AnonymizeErasedUserInvitationsAllTenants(ctx, userID, replacement); err != nil {
		return false, fmt.Errorf("anonymize invitations: %w", err)
	}

	var anonymized bool
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if tx.Privacy == nil {
			return apperror.NewInternal("privacy services are not configured")
		}
		latest, err := tx.Memberships.ListForUser(ctx, userID)
		if err != nil {
			return apperror.NewInternal("failed to load memberships")
		}
		if !allMembershipsRemoved(latest) {
			return errErasurePending
		}
		anonymized, err = s.cleanupUser(ctx, tx, userID)
		return err
	})
	return anonymized, err
}

func allMembershipsRemoved(memberships []Membership) bool {
	for i := range memberships {
		if memberships[i].Status != MembershipStatusRemoved {
			return false
		}
	}
	return true
}

func (s *PrivacyService) cleanupUser(ctx context.Context, tx TxRepos, userID int64) (bool, error) {
	anonymized := false
	if _, err := tx.Privacy.AnonymizeUser(ctx, userID, erasedEmail(userID)); err != nil {
		if !errors.Is(err, apperror.ErrNotFound) {
			return false, apperror.NewInternal("failed to anonymize user")
		}
	} else {
		anonymized = true
	}
	// Always finish the cleanup: an account deleted by the older self-service
	// path may already be status=deleted but still hold credentials,
	// identities, sessions, and tokens.
	if err := tx.Privacy.DeleteCredentialsAndIdentities(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete credentials")
	}
	if err := tx.Privacy.DeleteSessionsForUser(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete sessions")
	}
	// The user row is anonymized, not deleted, so the MFA tables' cascade
	// never fires: remove the sealed TOTP secret and recovery codes here.
	if err := tx.MFA.DeleteTOTP(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete MFA factor")
	}
	if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete recovery codes")
	}
	if err := tx.Resets.DeleteByUser(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete reset tokens")
	}
	if err := tx.Verifications.DeleteByUser(ctx, userID); err != nil {
		return false, apperror.NewInternal("failed to delete verification tokens")
	}
	if !anonymized {
		return false, nil
	}
	return true, checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.erased", userID, map[string]any{"scope": "account"})))
}

// HandlePurgeJob deletes expired export blobs, then their rows.
func (s *PrivacyService) HandlePurgeJob(ctx context.Context, _ json.RawMessage) error {
	budgetCtx, cancel := context.WithTimeout(ctx, exportPurgeBudget)
	defer cancel()
	for {
		exports, err := s.stores.Privacy.Purgeable(budgetCtx, exportPurgeBatchSize)
		if err != nil {
			return fmt.Errorf("list expiring data exports: %w", err)
		}
		for _, export := range exports {
			if export.StoragePath != "" {
				if err := s.storage.Delete(budgetCtx, export.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("delete data export %d: %w", export.ID, err)
				}
			}
			if err := s.stores.Privacy.Delete(budgetCtx, export.ID); err != nil {
				return fmt.Errorf("delete data export row %d: %w", export.ID, err)
			}
		}
		if len(exports) < exportPurgeBatchSize {
			return nil
		}
		if err := budgetCtx.Err(); err != nil {
			slog.Warn("data export purge budget exhausted; the next run continues")
			return nil
		}
	}
}

func erasedEmail(userID int64) string {
	return fmt.Sprintf("erased+%d@invalid", userID)
}

var _ privacycontracts.Exporter = (*PrivacyService)(nil)
var _ privacycontracts.Eraser = (*PrivacyService)(nil)
