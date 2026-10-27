package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
)

const (
	sqlStateForeignKeyViolation = "23503"
	sqlStateRestrictViolation   = "23001"
)

// RoleRepository implements core.RoleRepository.
type RoleRepository struct {
	q *identitysqlc.Queries
}

var _ core.RoleRepository = (*RoleRepository)(nil)

func NewRoleRepository(db identitysqlc.DBTX) *RoleRepository {
	return &RoleRepository{q: identitysqlc.New(db)}
}

func toRole(id uuid.UUID, tenantID uuid.NullUUID, scope, key, name string,
	permissions []string, createdAt, updatedAt pgtype.Timestamptz,
) core.Role {
	role := core.Role{
		ID:          id,
		Scope:       scope,
		Key:         key,
		Name:        name,
		Permissions: permissions,
		CreatedAt:   createdAt.Time,
		UpdatedAt:   updatedAt.Time,
	}
	role.TenantID = tenantID
	return role
}

func (r *RoleRepository) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]core.Role, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListRolesCursor(ctx, identitysqlc.ListRolesCursorParams{
		TenantID:        tenantID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	roles := make([]core.Role, len(rows))
	for i := range rows {
		roles[i] = toRole(rows[i].ID, rows[i].TenantID, rows[i].Scope, rows[i].Key,
			rows[i].Name, rows[i].Permissions, rows[i].CreatedAt, rows[i].UpdatedAt)
	}
	return roles, nil
}

func (r *RoleRepository) Get(ctx context.Context, id uuid.UUID) (*core.Role, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetRole(ctx, identitysqlc.GetRoleParams{ID: id, TenantID: tenantID})
	if err != nil {
		return nil, wrapErr(err)
	}
	role := toRole(row.ID, row.TenantID, row.Scope, row.Key, row.Name,
		row.Permissions, row.CreatedAt, row.UpdatedAt)
	return &role, nil
}

func (r *RoleRepository) GetByKey(ctx context.Context, key string) (*core.Role, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: key, TenantID: tenantID})
	if err != nil {
		return nil, wrapErr(err)
	}
	role := toRole(row.ID, row.TenantID, row.Scope, row.Key, row.Name,
		row.Permissions, row.CreatedAt, row.UpdatedAt)
	return &role, nil
}

func (r *RoleRepository) Create(ctx context.Context, in core.NewRole) (*core.Role, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.CreateRole(ctx, identitysqlc.CreateRoleParams{
		TenantID: uuid.NullUUID{UUID: tenantID, Valid: true},
		Key:      in.Key,
		Name:     in.Name,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == "uq_roles_tenant_key" {
			return nil, core.ErrRoleKeyTaken
		}
		return nil, wrapErr(err)
	}
	if err := r.replacePermissions(ctx, row.ID, in.Permissions); err != nil {
		return nil, err
	}
	role := toRole(row.ID, row.TenantID, row.Scope, row.Key, row.Name,
		in.Permissions, row.CreatedAt, row.UpdatedAt)
	return &role, nil
}

func (r *RoleRepository) Update(ctx context.Context, id uuid.UUID, name string, permissions []string, expected *time.Time) (*core.Role, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var row identitysqlc.Role
	if expected != nil {
		row, err = r.q.UpdateRoleIfUnchanged(ctx, identitysqlc.UpdateRoleIfUnchangedParams{
			ID:                id,
			TenantID:          uuid.NullUUID{UUID: tenantID, Valid: true},
			Name:              name,
			ExpectedUpdatedAt: timestamptzValue(*expected),
		})
	} else {
		row, err = r.q.UpdateRole(ctx, identitysqlc.UpdateRoleParams{
			ID:       id,
			TenantID: uuid.NullUUID{UUID: tenantID, Valid: true},
			Name:     name,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperror.NewPreconditionFailed("the role has changed since it was read; refresh and retry")
		}
		return nil, wrapErr(err)
	}
	if err := r.replacePermissions(ctx, row.ID, permissions); err != nil {
		return nil, err
	}
	role := toRole(row.ID, row.TenantID, row.Scope, row.Key, row.Name,
		permissions, row.CreatedAt, row.UpdatedAt)
	return &role, nil
}

func (r *RoleRepository) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.DeleteRole(ctx, identitysqlc.DeleteRoleParams{ID: id, TenantID: uuid.NullUUID{UUID: tenantID, Valid: true}})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == sqlStateForeignKeyViolation || pgErr.Code == sqlStateRestrictViolation) {
			return false, core.ErrRoleInUse
		}
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *RoleRepository) MemberIDs(ctx context.Context, roleID uuid.UUID) ([]uuid.UUID, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := r.q.RoleMemberIDs(ctx, identitysqlc.RoleMemberIDsParams{
		TenantID: tenantID, RoleID: roleID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return ids, nil
}

func (r *RoleRepository) ReplaceMemberRoles(ctx context.Context, userID uuid.UUID, roleKeys []string) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tid := tenantID
	if _, err := r.q.LockMembership(ctx, identitysqlc.LockMembershipParams{TenantID: tid, UserID: userID}); err != nil {
		return wrapErr(err)
	}
	if err := r.q.LockMembershipRoles(ctx, identitysqlc.LockMembershipRolesParams{
		TenantID: tid, UserID: userID,
	}); err != nil {
		return wrapErr(err)
	}
	roleIDs := make([]uuid.UUID, 0, len(roleKeys))
	for _, key := range roleKeys {
		row, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: key, TenantID: tid})
		if err != nil {
			if errors.Is(wrapErr(err), apperror.ErrNotFound) {
				return core.ErrRoleNotFound
			}
			return wrapErr(err)
		}
		roleIDs = append(roleIDs, row.ID)
	}
	if err := r.q.DeleteMemberRolesExcept(ctx, identitysqlc.DeleteMemberRolesExceptParams{
		TenantID: tid, UserID: userID, KeepRoleIds: roleIDs,
	}); err != nil {
		return wrapErr(err)
	}
	for _, roleID := range roleIDs {
		if err := r.q.InsertMembershipRole(ctx, identitysqlc.InsertMembershipRoleParams{
			TenantID: tid, UserID: userID, RoleID: roleID,
		}); err != nil {
			return wrapErr(err)
		}
	}
	return nil
}

func (r *RoleRepository) AssignPlatformRole(ctx context.Context, userID uuid.UUID, roleKey string) error {
	if err := r.q.AssignPlatformRole(ctx, identitysqlc.AssignPlatformRoleParams{
		UserID: userID, RoleKey: roleKey,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *RoleRepository) SyncCatalog(ctx context.Context, permissions []core.CatalogPermission,
	roles []core.CatalogRole,
) error {
	if err := r.q.LockPermissionSync(ctx); err != nil {
		return wrapErr(err)
	}
	keep := make([]string, len(permissions))
	for i, permission := range permissions {
		keep[i] = permission.Key
		if err := r.q.UpsertPermission(ctx, identitysqlc.UpsertPermissionParams{
			Key: permission.Key, Owner: permission.Owner,
		}); err != nil {
			return wrapErr(err)
		}
	}
	if err := r.q.PruneRolePermissionsOutsideCatalog(ctx, keep); err != nil {
		return wrapErr(err)
	}
	if err := r.q.PrunePermissions(ctx, keep); err != nil {
		return wrapErr(err)
	}

	for _, role := range roles {
		row, err := r.q.UpsertSystemRole(ctx, identitysqlc.UpsertSystemRoleParams{
			Scope: role.Scope, Key: role.Key, Name: role.Name,
		})
		if err != nil {
			return wrapErr(err)
		}
		if err := r.replacePermissions(ctx, row.ID, role.Permissions); err != nil {
			return err
		}
	}
	return nil
}

// replacePermissions upserts the set and prunes the rest, so concurrent
// replicas converge on the same rows without delete/insert races.
func (r *RoleRepository) replacePermissions(ctx context.Context, roleID uuid.UUID, permissions []string) error {
	keep := permissions
	if keep == nil {
		keep = []string{}
	}
	for _, permission := range keep {
		if err := r.q.UpsertRolePermission(ctx, identitysqlc.UpsertRolePermissionParams{
			RoleID: roleID, PermissionKey: permission,
		}); err != nil {
			return wrapErr(err)
		}
	}
	if err := r.q.PruneRolePermissions(ctx, identitysqlc.PruneRolePermissionsParams{
		RoleID: roleID, KeepKeys: keep,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

// timestamptzValue wraps a non-zero time for a conditional statement.
func timestamptzValue(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
