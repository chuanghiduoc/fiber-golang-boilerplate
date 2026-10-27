package identity

import (
	"time"

	"github.com/google/uuid"
)

// Domain events published by the identity module. The envelope
// (id, type, dataVersion, occurredAt, tenantId, actor) is
// internal/platform/events.Event; these structs are its data. They are public
// webhook contracts, deliberately separate from the REST DTOs: evolve them
// additively, and bump the version constant (or add a new type) for any
// breaking change.
const (
	// EventUserRegistered is published when a local account is created.
	EventUserRegistered = "user.registered"
	// UserRegisteredVersion is the current UserRegistered data version.
	UserRegisteredVersion = 1
)

// UserRegistered is the data of user.registered, version 1. The tenant and
// the actor (the new user) are in the envelope.
type UserRegistered struct {
	UserID        uuid.UUID `json:"userId"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
}

const (
	// EventDataExportReady is published when a user's data export has been
	// written and is downloadable. Notifications consumes it durably.
	EventDataExportReady = "identity.data_export.ready"
	// DataExportReadyVersion is the current DataExportReady data version.
	DataExportReadyVersion = 1
)

// DataExportReady is the data of identity.data_export.ready, version 1. The
// envelope carries the tenant and the requesting user as the actor.
type DataExportReady struct {
	ExportID uuid.UUID `json:"exportId"`
	UserID   uuid.UUID `json:"userId"`
}
