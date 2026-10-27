package dto

import (
	"time"

	"github.com/google/uuid"
)

// CreateTenantRequest creates a tenant from the platform surface.
type CreateTenantRequest struct {
	Slug        string     `json:"slug" required:"true" minLength:"2" maxLength:"63"`
	Name        string     `json:"name" required:"true" minLength:"1" maxLength:"255"`
	Status      string     `json:"status,omitempty" enum:"trialing,active"`
	TrialEndsAt *time.Time `json:"trialEndsAt,omitempty"`
	Locale      string     `json:"locale,omitempty" maxLength:"35"`
	Timezone    string     `json:"timezone,omitempty" maxLength:"64"`
}

// TenantResponse is one tenant row.
type TenantResponse struct {
	ID                  uuid.UUID  `json:"id"`
	Slug                string     `json:"slug"`
	Name                string     `json:"name"`
	Status              string     `json:"status" enum:"trialing,active,suspended,pending_deletion,deleted"`
	StatusChangedAt     time.Time  `json:"statusChangedAt"`
	TrialEndsAt         *time.Time `json:"trialEndsAt,omitempty"`
	DeletionScheduledAt *time.Time `json:"deletionScheduledAt,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}
