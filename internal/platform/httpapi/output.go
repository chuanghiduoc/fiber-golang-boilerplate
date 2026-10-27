package httpapi

import "github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"

// Output is a bare 200 response body.
type Output[T any] struct {
	Body T
}

// Created is a 201 response with the canonical Location header. Build it with
// NewCreated so Status is set; a zero Status would otherwise be 0 on the wire.
type Created[T any] struct {
	Status   int    `json:"-"`
	Location string `header:"Location" doc:"URI of the created resource."`
	Body     T
}

// NewCreated constructs a 201 response body.
func NewCreated[T any](location string, body T) *Created[T] {
	return &Created[T]{Status: 201, Location: location, Body: body}
}

// Accepted is a 202 response whose Location names the status resource for
// durable work (data export, erasure request, webhook replay). Build it with
// NewAccepted.
type Accepted[T any] struct {
	Status   int    `json:"-"`
	Location string `header:"Location" doc:"URI of the status resource."`
	Body     T
}

// NewAccepted constructs a 202 response body.
func NewAccepted[T any](location string, body T) *Accepted[T] {
	return &Accepted[T]{Status: 202, Location: location, Body: body}
}

// Empty is a 204 response with no body. Its zero value is the whole output.
type Empty struct{}

// ListOutput wraps the cursor envelope as a huma 200 body.
type ListOutput[T any] struct {
	Body pagination.List[T]
}

// ETagOutput is a 200 resource with an optimistic-concurrency validator.
type ETagOutput[T any] struct {
	ETag string `header:"ETag" doc:"Opaque version for If-Match."`
	Body T
}

// HTTPStatus lets Register document the response status of the helper type.
func (*Created[T]) HTTPStatus() int { return 201 }

// HTTPStatus lets Register document the response status of the helper type.
func (*Accepted[T]) HTTPStatus() int { return 202 }

// HTTPStatus lets Register document the response status of the helper type.
func (*Empty) HTTPStatus() int { return 204 }
