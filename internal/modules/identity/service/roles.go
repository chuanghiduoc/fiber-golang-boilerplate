package service

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// roleKeyPattern matches tenant role keys: lower-case, digits, underscore.
var roleKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,49}$`)

// reservedRoleKeys are the system role keys a tenant role may not shadow.
var reservedRoleKeys = map[string]struct{}{
	authz.RoleOwner: {}, authz.RoleAdmin: {}, authz.RoleMember: {}, authz.RolePlatformAdmin: {},
}

// RoleService manages tenant role definitions and member role assignments.
type RoleService interface {
	List(ctx context.Context, limit int, startingAfter string) ([]Role, bool, error)
	Create(ctx context.Context, in NewRole) (*Role, error)
	Update(ctx context.Context, id int64, in RoleUpdate) (*Role, error)
	Delete(ctx context.Context, id int64) error
	ReplaceMemberRoles(ctx context.Context, userID int64, roleKeys []string) (*Member, error)
}

type roleService struct {
	stores Stores
	tx     Transactor
	cache  session.Store
}

// NewRoleService wires the role service. cache may be nil.
func NewRoleService(stores Stores, tx Transactor, cache session.Store) RoleService {
	return &roleService{stores: stores, tx: tx, cache: cache}
}

func (s *roleService) List(ctx context.Context, limit int, startingAfter string) ([]Role, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	roles, err := s.stores.Roles.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list roles")
	}
	hasMore := len(roles) > pageSize
	if hasMore {
		roles = roles[:pageSize]
	}
	return roles, hasMore, nil
}

func (s *roleService) Create(ctx context.Context, in NewRole) (*Role, error) {
	in.Key = strings.TrimSpace(in.Key)
	in.Name = strings.TrimSpace(in.Name)
	if !roleKeyPattern.MatchString(in.Key) {
		return nil, apperror.NewBadRequest("role key must be lower-case letters, digits, or underscores")
	}
	if _, reserved := reservedRoleKeys[in.Key]; reserved {
		return nil, apperror.NewConflict("role key is reserved")
	}
	if in.Name == "" {
		return nil, apperror.NewBadRequest("role name is required")
	}
	permissions, err := normalizePermissions(in.Permissions)
	if err != nil {
		return nil, err
	}
	if err := authorizeGrant(ctx, permissions); err != nil {
		return nil, err
	}
	in.Permissions = permissions

	var created *Role
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		role, err := tx.Roles.Create(ctx, in)
		if err != nil {
			if errors.Is(err, ErrRoleKeyTaken) {
				return apperror.NewConflict("role key already exists")
			}
			return apperror.NewInternal("failed to create role")
		}
		created = role
		return checkAudit(tx.Audit.RecordTx(ctx, roleEvent(ctx, "admin.role.created", role, nil)))
	})
	if err != nil {
		return nil, err
	}
	s.invalidatePermissions(ctx)
	return created, nil
}

func (s *roleService) Update(ctx context.Context, id int64, in RoleUpdate) (*Role, error) {
	in.ID = id
	var updated *Role
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		existing, err := tx.Roles.Get(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("role not found")
			}
			return apperror.NewInternal("failed to load role")
		}
		if existing.System() {
			return apperror.NewBadRequest("system roles cannot be modified")
		}

		name := existing.Name
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
			if name == "" {
				return apperror.NewBadRequest("role name is required")
			}
		}
		permissions := existing.Permissions
		if in.Permissions != nil {
			normalized, err := normalizePermissions(*in.Permissions)
			if err != nil {
				return err
			}
			if err := authorizeGrant(ctx, addedPermissions(existing.Permissions, normalized)); err != nil {
				return err
			}
			permissions = normalized
		}

		role, err := tx.Roles.Update(ctx, id, name, permissions)
		if err != nil {
			return apperror.NewInternal("failed to update role")
		}
		updated = role
		return checkAudit(tx.Audit.RecordTx(ctx, roleEvent(ctx, "admin.role.updated", role, nil)))
	})
	if err != nil {
		return nil, err
	}
	s.invalidatePermissions(ctx)
	return updated, nil
}

func (s *roleService) Delete(ctx context.Context, id int64) error {
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		role, err := tx.Roles.Get(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("role not found")
			}
			return apperror.NewInternal("failed to load role")
		}
		if role.System() {
			return apperror.NewBadRequest("system roles cannot be deleted")
		}
		deleted, err := tx.Roles.Delete(ctx, id)
		if err != nil {
			if errors.Is(err, ErrRoleInUse) {
				return apperror.NewConflict("role is still assigned to a member")
			}
			return apperror.NewInternal("failed to delete role")
		}
		if !deleted {
			return apperror.NewNotFound("role not found")
		}
		return checkAudit(tx.Audit.RecordTx(ctx, roleEvent(ctx, "admin.role.deleted", role, nil)))
	})
	if err != nil {
		return err
	}
	s.invalidatePermissions(ctx)
	return nil
}

func (s *roleService) ReplaceMemberRoles(ctx context.Context, userID int64, roleKeys []string) (*Member, error) {
	keys, err := normalizeRoleKeys(roleKeys)
	if err != nil {
		return nil, err
	}

	var member *Member
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		updated, err := replaceMemberRolesTx(ctx, tx, userID, keys)
		if err != nil {
			return err
		}
		member = updated
		event := userEvent(ctx, "admin.user.roles_replaced", userID, map[string]any{"roles": keys})
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		return nil, err
	}
	invalidateSessions(ctx, s.cache, userID)
	return member, nil
}

func replaceMemberRolesTx(ctx context.Context, tx TxRepos, userID int64, keys []string) (*Member, error) {
	existing, err := tx.Memberships.GetActive(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	granted, err := resolveRolePermissions(ctx, tx.Roles, keys)
	if err != nil {
		return nil, err
	}
	if err := authorizeGrant(ctx, granted); err != nil {
		return nil, err
	}
	if existing.Membership.Role == RoleOwner && !containsKey(keys, RoleOwner) {
		if err := ensureNotLastOwner(ctx, tx.Memberships, "cannot remove the last active owner"); err != nil {
			return nil, err
		}
	}
	if err := tx.Roles.ReplaceMemberRoles(ctx, userID, keys); err != nil {
		if errors.Is(err, ErrRoleNotFound) {
			return nil, apperror.NewBadRequest("unknown role")
		}
		return nil, err
	}
	if err := tx.Sessions.RevokeAllInTenant(ctx, userID, RevokeMembershipChanged); err != nil {
		return nil, apperror.NewInternal("failed to revoke sessions")
	}
	updated, err := tx.Memberships.GetActive(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to get user")
	}
	return updated, nil
}

// resolveRolePermissions validates the keys and returns every permission the
// target roles grant.
func resolveRolePermissions(ctx context.Context, roles RoleRepository, keys []string) ([]string, error) {
	permissions := make([]string, 0, len(keys))
	for _, key := range keys {
		role, err := roles.GetByKey(ctx, key)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil, apperror.NewBadRequest("unknown role " + key)
			}
			return nil, apperror.NewInternal("failed to load role")
		}
		permissions = append(permissions, role.Permissions...)
	}
	return permissions, nil
}

// addedPermissions returns the permissions in next that before does not hold.
func addedPermissions(before, next []string) []string {
	existing := make(map[string]struct{}, len(before))
	for _, permission := range before {
		existing[permission] = struct{}{}
	}
	added := make([]string, 0, len(next))
	for _, permission := range next {
		if _, ok := existing[permission]; !ok {
			added = append(added, permission)
		}
	}
	return added
}

// authorizeGrant keeps a caller from granting permissions it does not hold
// itself. HTTP requests always carry a subject; an internal/system caller
// without one has already passed the trusted boundary.
func authorizeGrant(ctx context.Context, permissions []string) error {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok {
		return nil
	}
	for _, permission := range permissions {
		if !subject.Has(authz.Permission(permission)) {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
	}
	return nil
}

func (s *roleService) invalidatePermissions(ctx context.Context) {
	if s.cache == nil {
		return
	}
	if err := s.cache.InvalidatePermissions(ctx); err != nil {
		slog.Warn("failed to invalidate cached permissions", slog.Any("error", err))
	}
}

// normalizeRoleKeys trims, deduplicates, and sorts role keys.
func normalizeRoleKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, apperror.NewBadRequest("at least one role is required")
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = NormalizeRole(strings.TrimSpace(key))
		if !roleKeyPattern.MatchString(key) {
			return nil, apperror.NewBadRequest("invalid role key " + key)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// normalizePermissions keeps only known tenant permissions.
func normalizePermissions(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, apperror.NewBadRequest("permissions must not contain empty values")
		}
		permission := authz.Permission(value)
		if !authz.IsKnownPermission(permission) {
			return nil, apperror.NewBadRequest("unknown permission " + value)
		}
		if authz.IsPlatformPermission(permission) {
			return nil, apperror.NewBadRequest("platform permissions cannot be granted to tenant roles")
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func containsKey(keys []string, key string) bool {
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

func roleEvent(ctx context.Context, action string, role *Role, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "role")
	event.ResourceID = strconv.FormatInt(role.ID, 10)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["key"] = role.Key
	event.Metadata = metadata
	return event
}
