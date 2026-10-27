package privacy

import (
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// roleSubject builds the subject a live session would carry for role: the
// explicit permission set from the catalog, never the bare role name.
func roleSubject(userID, tenantID uuid.UUID, role string) authz.Subject {
	granted := fakes.Catalog.PermissionsForRole(role)
	permissions := make([]authz.Permission, 0, len(granted))
	for permission := range granted {
		permissions = append(permissions, permission)
	}
	return authz.NewSubjectWithPermissions(userID, tenantID, role, permissions...)
}
