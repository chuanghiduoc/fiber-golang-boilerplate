package dto

import "time"

// CreateTenantRequest creates a tenant from the platform surface.
type CreateTenantRequest struct {
	Slug        string     `json:"slug" validate:"required,min=2,max=63"`
	Name        string     `json:"name" validate:"required,min=1,max=255,notblank"`
	Status      string     `json:"status" validate:"omitempty,oneof=trialing active"`
	TrialEndsAt *time.Time `json:"trialEndsAt"`
	Locale      string     `json:"locale" validate:"omitempty,max=35"`
	Timezone    string     `json:"timezone" validate:"omitempty,max=64"`
}

// UpdateTenantStatusRequest moves a tenant through its lifecycle.
type UpdateTenantStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=trialing active suspended pending_deletion deleted"`
}

// TenantResponse is one tenant row.
type TenantResponse struct {
	ID                  int64      `json:"id"`
	Slug                string     `json:"slug"`
	Name                string     `json:"name"`
	Status              string     `json:"status" enums:"trialing,active,suspended,pending_deletion,deleted"`
	StatusChangedAt     time.Time  `json:"statusChangedAt"`
	TrialEndsAt         *time.Time `json:"trialEndsAt,omitempty"`
	DeletionScheduledAt *time.Time `json:"deletionScheduledAt,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}
