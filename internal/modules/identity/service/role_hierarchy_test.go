package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestRoleRank(t *testing.T) {
	for role, want := range map[string]int{
		RoleOwner:  3,
		RoleAdmin:  2,
		RoleMember: 1,
		"auditor":  1,
		"":         1,
	} {
		assert.Equal(t, want, roleRank(role), role)
	}
}

func TestHighestRoleRank(t *testing.T) {
	for name, tc := range map[string]struct {
		roles []string
		want  int
	}{
		"owner wins":       {roles: []string{RoleMember, RoleOwner, RoleAdmin}, want: 3},
		"admin":            {roles: []string{"auditor", RoleAdmin}, want: 2},
		"custom is member": {roles: []string{"auditor"}, want: 1},
		"empty is member":  {roles: nil, want: 1},
	} {
		assert.Equal(t, tc.want, highestRoleRank(tc.roles), name)
	}
}

// stubRoleRepo resolves the role definitions a rank check needs; unused
// methods panic if called.
type stubRoleRepo struct {
	RoleRepository
	roles map[string]*Role
}

func (s stubRoleRepo) GetByKey(_ context.Context, key string) (*Role, error) {
	if role, ok := s.roles[key]; ok {
		return role, nil
	}
	return nil, apperror.ErrNotFound
}

func TestHighestEffectiveRank(t *testing.T) {
	repo := stubRoleRepo{roles: map[string]*Role{
		RoleOwner:  {Key: RoleOwner},
		RoleAdmin:  {Key: RoleAdmin},
		RoleMember: {Key: RoleMember, Permissions: []string{"profile:read"}},
		"delegated_role": {
			Key: "delegated_role", Permissions: []string{string(authz.PermissionAdminUsersRole)},
		},
		"delegated_ban": {
			Key: "delegated_ban", Permissions: []string{string(authz.PermissionAdminUsersBan)},
		},
		"delegated_delete": {
			Key: "delegated_delete", Permissions: []string{string(authz.PermissionAdminUsersDelete)},
		},
		"auditor": {Key: "auditor", Permissions: []string{string(authz.PermissionAdminUsersRead)}},
		"reader":  {Key: "reader", Permissions: []string{"profile:read"}},
	}}
	for name, tc := range map[string]struct {
		roles []string
		want  int
	}{
		"owner":                 {roles: []string{RoleOwner}, want: 3},
		"admin":                 {roles: []string{RoleAdmin}, want: 2},
		"member":                {roles: []string{RoleMember}, want: 1},
		"empty is member":       {roles: nil, want: 1},
		"custom role is member": {roles: []string{"reader"}, want: 1},
		"read-only admin perm":  {roles: []string{"auditor"}, want: 1},
		"role-manager":          {roles: []string{"delegated_role"}, want: 2},
		"banner":                {roles: []string{"delegated_ban"}, want: 2},
		"remover":               {roles: []string{"delegated_delete"}, want: 2},
		"highest wins":          {roles: []string{"reader", "delegated_role"}, want: 2},
		"unknown key":           {roles: []string{"gone-role"}, want: 1},
	} {
		got, err := highestEffectiveRank(context.Background(), repo, tc.roles)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
}

func TestEffectiveRoleRank(t *testing.T) {
	assert.Equal(t, 3, effectiveRoleRank(&Role{Key: RoleOwner}))
	assert.Equal(t, 2, effectiveRoleRank(&Role{Key: RoleAdmin}))
	assert.Equal(t, 2, effectiveRoleRank(&Role{Key: "custom", Permissions: []string{"admin:users:delete"}}))
	assert.Equal(t, 1, effectiveRoleRank(&Role{Key: "custom", Permissions: []string{"admin:users:read"}}))
	assert.Equal(t, 1, effectiveRoleRank(&Role{Key: "custom"}))
}

func TestCanManageRank(t *testing.T) {
	for name, tc := range map[string]struct {
		caller int
		target int
		want   bool
	}{
		"owner manages owner":  {caller: 3, target: 3, want: true},
		"owner manages admin":  {caller: 3, target: 2, want: true},
		"owner manages member": {caller: 3, target: 1, want: true},
		"admin manages owner":  {caller: 2, target: 3, want: false},
		"admin manages admin":  {caller: 2, target: 2, want: false},
		"admin manages member": {caller: 2, target: 1, want: true},
		"custom manages peer":  {caller: 1, target: 1, want: false},
		"member manages admin": {caller: 1, target: 2, want: false},
	} {
		assert.Equal(t, tc.want, canManageRank(tc.caller, tc.target), name)
	}
}

func TestCanGrantRank(t *testing.T) {
	for name, tc := range map[string]struct {
		caller int
		role   int
		want   bool
	}{
		"owner grants owner":   {caller: 3, role: 3, want: true},
		"owner grants admin":   {caller: 3, role: 2, want: true},
		"admin grants owner":   {caller: 2, role: 3, want: false},
		"admin grants admin":   {caller: 2, role: 2, want: true},
		"admin grants custom":  {caller: 2, role: 1, want: true},
		"member grants custom": {caller: 1, role: 1, want: true},
	} {
		assert.Equal(t, tc.want, canGrantRank(tc.caller, tc.role), name)
	}
}

// authorizeGrant used to return nil whenever the context carried no subject.
// An internal caller must now say so explicitly with authz.SystemContext.
func TestAuthorizeGrantFailsClosedWithoutSubject(t *testing.T) {
	err := authorizeGrant(context.Background(), []string{"invitations:read"})
	assert.Equal(t, http.StatusForbidden, statusOf(err))
	assert.Equal(t, string(apperror.CodePermissionDenied), codeOf(err))

	assert.NoError(t, authorizeGrant(authz.SystemContext(context.Background()), []string{"invitations:read"}))

	subjectCtx := authz.WithSubject(context.Background(), authz.NewSubject(7, 1, RoleMember))
	assert.NoError(t, authorizeGrant(subjectCtx, []string{"profile:read"}))
	assert.Equal(t, http.StatusForbidden, statusOf(authorizeGrant(subjectCtx, []string{"admin:users:role"})))
}
