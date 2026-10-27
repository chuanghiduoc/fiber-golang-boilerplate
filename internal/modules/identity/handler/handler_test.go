package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
	"github.com/google/uuid"
)

type updateProfileUsers struct {
	service.UserService
	member   *service.Member
	password string
	updated  service.UpdateProfileInput
}

func (s *updateProfileUsers) GetByID(context.Context, uuid.UUID) (*service.Member, error) {
	return s.member, nil
}

func (s *updateProfileUsers) VerifyPassword(_ context.Context, _ uuid.UUID, password string) error {
	if password != s.password {
		return apperror.NewBadRequest("current password is incorrect")
	}
	return nil
}

func (s *updateProfileUsers) Update(_ context.Context, _ uuid.UUID, in service.UpdateProfileInput) (*service.Member, error) {
	s.updated = in
	return s.member, nil
}

func profileTestContext(userID uuid.UUID) context.Context {
	return authz.WithSubject(context.Background(), authz.NewSubject(userID, testID(1), authz.RoleMember))
}

func TestUpdateMe_MergePatchAppliesOnlyPresentMembers(t *testing.T) {
	userID := testID(1)
	users := &updateProfileUsers{
		member:   &service.Member{User: service.User{ID: userID, Email: "old@example.com", Name: "Old"}},
		password: handlerTestPassword,
	}
	handler := NewUserHandler(users)

	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		RawBody: []byte(`{"name":"New Name"}`),
	})
	require.NoError(t, err)
	require.NotNil(t, users.updated.Name)
	require.Equal(t, "New Name", *users.updated.Name)
	require.Nil(t, users.updated.Email, "an absent email must stay unchanged")
}

func TestUpdateMe_EmailChangeRequiresCurrentPassword(t *testing.T) {
	userID := testID(1)
	users := &updateProfileUsers{
		member:   &service.Member{User: service.User{ID: userID, Email: "old@example.com", Name: "Old", HasPassword: true}},
		password: handlerTestPassword,
	}
	handler := NewUserHandler(users)

	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		RawBody: []byte(`{"email":"new@example.com"}`),
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 400, resolvedStatus(appErr))

	_, err = handler.UpdateMe(profileTestContext(userID), &updateProfileInput{
		RawBody: []byte(`{"email":"new@example.com","currentPassword":"` + handlerWrongPassword + `"}`),
	})
	require.Error(t, err)
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 400, resolvedStatus(appErr))
}

func TestUpdateMe_ExplicitNullIsRejected(t *testing.T) {
	userID := testID(1)
	handler := NewUserHandler(&updateProfileUsers{member: &service.Member{User: service.User{ID: userID}}})
	_, err := handler.UpdateMe(profileTestContext(userID), &updateProfileInput{RawBody: []byte(`{"name":null}`)})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 422, resolvedStatus(appErr))
}

type roleTestService struct {
	service.RoleService
	role       *service.Role
	updated    bool
	lastUpdate service.RoleUpdate
}

func (s *roleTestService) Get(context.Context, uuid.UUID) (*service.Role, error) {
	return s.role, nil
}

func (s *roleTestService) Update(_ context.Context, _ uuid.UUID, in service.RoleUpdate) (*service.Role, error) {
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
	roles := &roleTestService{role: &service.Role{ID: testID(9), Key: "auditor", Name: "Auditor", UpdatedAt: updatedAt}}
	handler := NewRoleHandler(roles)

	_, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID:  testID(9).String(),
		IfMatch: `"stale"`,
		RawBody: []byte(`{"name":"New"}`),
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, 412, resolvedStatus(appErr))
	require.False(t, roles.updated, "a stale precondition must not mutate")
}

func TestUpdateRole_CurrentIfMatchAppliesPatch(t *testing.T) {
	updatedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	roles := &roleTestService{role: &service.Role{ID: testID(9), Key: "auditor", Name: "Auditor", UpdatedAt: updatedAt}}
	handler := NewRoleHandler(roles)

	out, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID:  testID(9).String(),
		IfMatch: etagHeader(updatedAt),
		RawBody: []byte(`{"name":"New"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "New", out.Body.Name)
	require.True(t, roles.updated)
}

func TestUpdateRole_ExplicitNullPermissionsClearsThem(t *testing.T) {
	roles := &roleTestService{role: &service.Role{ID: testID(9), Key: "auditor", Name: "Auditor", Permissions: []string{"profile:read"}}}
	handler := NewRoleHandler(roles)
	_, err := handler.UpdateRole(context.Background(), &updateRoleInput{
		RoleID:  testID(9).String(),
		RawBody: []byte(`{"permissions":null}`),
	})
	require.NoError(t, err)
	require.NotNil(t, roles.lastUpdate.Permissions)
	require.Empty(t, *roles.lastUpdate.Permissions)
}

func TestProfilePatch_DecodesAbsentAndNull(t *testing.T) {
	var req dto.ProfilePatch
	require.NoError(t, decodeMergePatch([]byte(`{"name":null}`), &req))
	require.True(t, req.Name.Present)
	require.True(t, req.Name.Null)
	require.False(t, req.Email.Present)

	var field patch.Field[string]
	require.NoError(t, field.UnmarshalJSON([]byte(`"x"`)))
	require.True(t, field.Present)
	require.False(t, field.Null)
	require.Equal(t, "x", field.Value)
}
