// Package featuresdto contains the feature-flag transport types.
package featuresdto

import (
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

// FeatureFlagResponse is the process-wide flag state plus the value the
// requesting caller observes after the full precedence chain. Enabled is the
// configured database default (or the compiled default when Configured is
// false, including a row that exists only for a kill switch or targets).
type FeatureFlagResponse struct {
	Key         string     `json:"key"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	KillSwitch  bool       `json:"killSwitch"`
	Configured  bool       `json:"configured"`
	Effective   bool       `json:"effective"`
	Source      string     `json:"source"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// EvaluatedFlagResponse is one flag's effective value for the request tenant.
type EvaluatedFlagResponse struct {
	Key        string `json:"key"`
	Enabled    bool   `json:"enabled"`
	KillSwitch bool   `json:"killSwitch"`
	Source     string `json:"source"`
}

// FeatureFlagTargetResponse is a tenant-wide (userId omitted) or user target.
// TargetID is the canonical item id, not the surrogate row id.
type FeatureFlagTargetResponse struct {
	TargetID  string     `json:"targetId"`
	FlagKey   string     `json:"flagKey"`
	TenantID  uuid.UUID  `json:"tenantId" format:"uuid"`
	UserID    *uuid.UUID `json:"userId,omitempty" format:"uuid"`
	Enabled   bool       `json:"enabled"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// CreateFeatureFlagRequest creates the process-wide row for a catalog key. The
// key is part of the body because the collection is the resource family; the
// item URI uses it afterwards.
type CreateFeatureFlagRequest struct {
	Key         string  `json:"key" required:"true" maxLength:"128"`
	Enabled     *bool   `json:"enabled" required:"false"`
	KillSwitch  *bool   `json:"killSwitch" required:"true"`
	Description *string `json:"description" required:"true" maxLength:"500"`
}

// MergeBool is one RFC 7396 boolean member: present-and-null clears the field,
// present-with-a-value replaces it, absent leaves it unchanged.
type MergeBool struct{ patch.Field[bool] }

// Schema implements huma.SchemaProvider.
func (MergeBool) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeBoolean, Nullable: true}
}

// MergeString is one RFC 7396 string member.
type MergeString struct{ patch.Field[string] }

// Schema implements huma.SchemaProvider.
func (MergeString) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Nullable: true}
}

// UpdateFeatureFlagRequest is a merge patch on a flag row. Nulling enabled
// clears the database default; nulling killSwitch clears it to false; nulling
// description restores the catalog description.
type UpdateFeatureFlagRequest struct {
	Enabled     MergeBool   `json:"enabled" required:"false"`
	KillSwitch  MergeBool   `json:"killSwitch" required:"false"`
	Description MergeString `json:"description" required:"false"`
}

// PutTargetRequest replaces the enabled state at a target item URI.
type PutTargetRequest struct {
	Enabled *bool `json:"enabled" required:"true"`
}
