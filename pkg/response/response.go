package response

import "github.com/gofiber/fiber/v3"

// ListResponse is the Stripe/Linear-style envelope for collection endpoints.
// totalCount/page/pageSize are intentionally omitted — COUNT(*) on large tables
// is a hot-path bottleneck; clients paginate via hasMore + the last item's cursor.
type ListResponse struct {
	Object  string `json:"object"`
	URL     string `json:"url"`
	Data    any    `json:"data"`
	HasMore bool   `json:"hasMore"`
}

// Success returns a single resource directly (no envelope), 200 OK.
func Success(c fiber.Ctx, data any) error {
	return c.Status(fiber.StatusOK).JSON(data)
}

// Created returns a single resource directly, 201 Created.
func Created(c fiber.Ctx, data any) error {
	return c.Status(fiber.StatusCreated).JSON(data)
}

// NoContent returns 204 with no body.
func NoContent(c fiber.Ctx) error {
	return c.SendStatus(fiber.StatusNoContent)
}

// List returns a collection wrapped in the Stripe-style list envelope. The url
// is taken from the request path so clients can echo it. data should be a
// non-nil slice so it serialises as [] rather than null.
func List(c fiber.Ctx, data any, hasMore bool) error {
	return c.Status(fiber.StatusOK).JSON(ListResponse{
		Object:  "list",
		URL:     c.Path(),
		Data:    data,
		HasMore: hasMore,
	})
}
