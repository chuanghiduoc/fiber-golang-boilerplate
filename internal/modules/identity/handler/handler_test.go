package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
	"github.com/google/uuid"
)

func decodePatch[T any](t *testing.T, body string) T {
	t.Helper()
	var out T
	require.NoError(t, httpapi.UnmarshalJSONStrict([]byte(body), &out))
	return out
}

type updateProfileUsers struct {
	authn.UserService
	member   *core.Member
	password string
	updated  authn.UpdateProfileInput
}

func (s *updateProfileUsers) GetByID(context.Context, uuid.UUID) (*core.Member, error) {
	return s.member, nil
}

func (s *updateProfileUsers) VerifyPassword(_ context.Context, _ uuid.UUID, password string) error {
	if password != s.password {
		return apperror.NewBadRequest("current password is incorrect")
	}
	return nil
}

func (s *updateProfileUsers) Update(_ context.Context, _ uuid.UUID, in authn.UpdateProfileInput) (*core.Member, error) {
	s.updated = in
	return s.member, nil
}

func profileTestContext(userID uuid.UUID) context.Context {
	return authz.WithSubject(context.Background(), authz.NewSubject(userID, testID(1), authz.RoleMember))
}

func TestUpdateMe_MergePatchAppliesOnlyPresentMembers(t *testing.T) {
	userID := testID(1)
	users := &updateProfileUsers{
		member:   &core.Member{User: core.User{ID: userID, Email: "old@example.com", Name: "Old"}},
		password: handlerTestPassword,
	}
	handler := NewUserHandler(users)

	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		Body: decodePatch[dto.ProfilePatch](t, `{"name":"New Name"}`),
	})
	require.NoError(t, err)
	require.NotNil(t, users.updated.Name)
	require.Equal(t, "New Name", *users.updated.Name)
	require.Nil(t, users.updated.Email, "an absent email must stay unchanged")
}

func TestUpdateMe_EmailChangeRequiresCurrentPassword(t *testing.T) {
	userID := testID(1)
	users := &updateProfileUsers{
		member:   &core.Member{User: core.User{ID: userID, Email: "old@example.com", Name: "Old", HasPassword: true}},
		password: handlerTestPassword,
	}
	handler := NewUserHandler(users)

	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		Body: decodePatch[dto.ProfilePatch](t, `{"email":"new@example.com"}`),
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 400, resolvedStatus(appErr))

	_, err = handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		Body: decodePatch[dto.ProfilePatch](t, `{"email":"new@example.com","currentPassword":"`+handlerWrongPassword+`"}`),
	})
	require.Error(t, err)
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 400, resolvedStatus(appErr))
}

func TestUpdateMe_ExplicitNullIsRejected(t *testing.T) {
	userID := testID(1)
	handler := NewUserHandler(&updateProfileUsers{member: &core.Member{User: core.User{ID: userID}}})
	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		Body: decodePatch[dto.ProfilePatch](t, `{"name":null}`),
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 422, resolvedStatus(appErr))
}

type roleTestService struct {
	membership.RoleService
	role       *core.Role
	updated    bool
	lastUpdate core.RoleUpdate
}

func (s *roleTestService) Get(context.Context, uuid.UUID) (*core.Role, error) {
	return s.role, nil
}

func (s *roleTestService) Update(_ context.Context, _ uuid.UUID, in core.RoleUpdate) (*core.Role, error) {
	s.updated = true
	s.lastUpdate = in
	role := *s.role
	if in.Permissions != nil {
		role.Permissions = *in.Permissions
	}
	if in.Name != nil {
		role.Name = *in.Name
	}
	role.UpdatedAt = role.UpdatedAt.Add(time.Minute)
	return &role, nil
}

func TestUpdateRole_StaleIfMatchReturnsPreconditionFailed(t *testing.T) {
	updatedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	roles := &roleTestService{role: &core.Role{ID: testID(9), Key: "auditor", Name: "Auditor", UpdatedAt: updatedAt}}
	handler := NewRoleHandler(roles)

	_, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID:  testID(9).String(),
		IfMatch: `"stale"`,
		Body:    decodePatch[dto.RolePatch](t, `{"name":"New"}`),
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 412, resolvedStatus(appErr))
	require.False(t, roles.updated, "a stale precondition must not mutate")
}

func TestUpdateRole_CurrentIfMatchAppliesPatch(t *testing.T) {
	updatedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	roles := &roleTestService{role: &core.Role{ID: testID(9), Key: "auditor", Name: "Auditor", UpdatedAt: updatedAt}}
	handler := NewRoleHandler(roles)

	out, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID:  testID(9).String(),
		IfMatch: etagHeader(updatedAt),
		Body:    decodePatch[dto.RolePatch](t, `{"name":"New"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "New", out.Body.Name)
	require.True(t, roles.updated)
}

func TestUpdateRole_ExplicitNullPermissionsClearsThem(t *testing.T) {
	roles := &roleTestService{role: &core.Role{ID: testID(9), Key: "auditor", Name: "Auditor", Permissions: []string{"profile:read"}}}
	handler := NewRoleHandler(roles)
	_, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID: testID(9).String(),
		Body:   decodePatch[dto.RolePatch](t, `{"permissions":null}`),
	})
	require.NoError(t, err)
	require.NotNil(t, roles.lastUpdate.Permissions)
	require.Empty(t, *roles.lastUpdate.Permissions)
}

func TestUpdateRole_EmptyPatchIsNoOp(t *testing.T) {
	roles := &roleTestService{role: &core.Role{ID: testID(9), Key: "auditor", Name: "Auditor"}}
	handler := NewRoleHandler(roles)
	out, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID: testID(9).String(),
		Body:   decodePatch[dto.RolePatch](t, `{}`),
	})
	require.NoError(t, err)
	require.False(t, roles.updated, "an empty merge patch must not update or audit the role")
	require.Equal(t, "Auditor", out.Body.Name)
}

func TestProfilePatch_DecodesAbsentAndNull(t *testing.T) {
	req := decodePatch[dto.ProfilePatch](t, `{"name":null}`)
	require.True(t, req.Name.Present)
	require.True(t, req.Name.Null)
	require.False(t, req.Email.Present)

	var field patch.Field[string]
	require.NoError(t, field.UnmarshalJSON([]byte(`"x"`)))
	require.True(t, field.Present)
	require.False(t, field.Null)
	require.Equal(t, "x", field.Value)
}

func TestMergePatch_RejectsUnknownMembersAndTrailingValues(t *testing.T) {
	var req dto.ProfilePatch
	err := httpapi.UnmarshalJSONStrict([]byte(`{"name":"x","extra":1}`), &req)
	require.Error(t, err)

	req = dto.ProfilePatch{}
	err = httpapi.UnmarshalJSONStrict([]byte(`{"name":"x"} {"name":"y"}`), &req)
	require.Error(t, err)
}
