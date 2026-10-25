package service

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"

// AttachEventBus wires domain event publishing (user.registered) without
// widening the UserService interface, so handler fakes remain small. A nil
// bus publishes nothing.
func AttachEventBus(svc UserService, bus *events.Bus) {
	if s, ok := svc.(*userService); ok {
		s.events = bus
	}
}
