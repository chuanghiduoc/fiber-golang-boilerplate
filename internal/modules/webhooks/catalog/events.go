package catalog

import (
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
)

// SubscribedEvents are the domain event types webhooks subscribes to durably
// and fans out to matching endpoints (ADR 0011). Each type's data struct and
// version live with its producer in internal/contracts/<producer>. Adding a
// type here makes it deliverable; endpoints still choose it by pattern.
func SubscribedEvents() []string {
	return []string{
		identitycontracts.EventUserRegistered,
		filescontracts.EventFileUploaded,
		filescontracts.EventFileDeleted,
		filescontracts.EventFileRestored,
	}
}

// EventHandlerName names the webhooks durable subscription in its job kind
// (events.<type>.webhooks) and idempotency key.
const EventHandlerName = "webhooks"
