// Package featuresdto contains the feature-flag admin transport types.
package featuresdto

import "time"

// UpsertFlagRequest replaces the database default and kill switch for a
// compile-time flag. The key comes from the path, never the body. Omitting
// enabled (or sending null) clears the database default so environment and
// compiled defaults apply again.
type UpsertFlagRequest struct {
	Enabled     *bool   `json:"enabled"`
	KillSwitch  *bool   `json:"killSwitch" validate:"required"`
	Description *string `json:"description" validate:"omitempty,max=500"`
}

// UpsertTargetRequest creates or replaces a tenant-wide target (when userId is
// omitted) or a user target (when userId is present). The tenant always comes
// from the request context, so callers cannot write another tenant's target.
type UpsertTargetRequest struct {
	UserID  *int64 `json:"userId" validate:"omitempty,min=1"`
	Enabled *bool  `json:"enabled" validate:"required"`
}

// FlagResponse is the admin view of one compile-time flag. Enabled is the
// configured database default (or the compiled default when Configured is
// false, including a row that exists only for a kill switch or targets);
// Effective is the value the requesting admin would observe after the
// full precedence chain.
type FlagResponse struct {
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

// FlagDetailResponse includes the flag and every target in the current tenant.
type FlagDetailResponse struct {
	Flag    FlagResponse     `json:"flag"`
	Targets []TargetResponse `json:"targets"`
}

// TargetResponse is a tenant-wide (userId omitted) or user target.
type TargetResponse struct {
	ID        int64      `json:"id"`
	FlagKey   string     `json:"flagKey"`
	TenantID  int64      `json:"tenantId"`
	UserID    *int64     `json:"userId,omitempty"`
	Enabled   bool       `json:"enabled"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}
