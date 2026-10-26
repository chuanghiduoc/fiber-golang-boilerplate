package identity

import "time"

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
	UserID        int64     `json:"userId"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
}
