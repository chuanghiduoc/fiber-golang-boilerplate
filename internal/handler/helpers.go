package handler

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// paramID extracts and validates a required int64 path parameter.
func paramID(c fiber.Ctx, name string) (int64, error) {
	id := fiber.Params[int64](c, name)
	if id == 0 {
		return 0, apperror.NewBadRequest("invalid " + name)
	}
	return id, nil
}

// authUserID returns the authenticated user's ID from the JWT context.
// Key is set by middleware.JWTAuth.
func authUserID(c fiber.Ctx) int64 {
	return fiber.Locals[int64](c, "user_id")
}

// authRole returns the authenticated user's role from the JWT context.
func authRole(c fiber.Ctx) string {
	return fiber.Locals[string](c, "role")
}

// bindAndValidate parses the request body and runs struct validation.
func bindAndValidate(c fiber.Ctx, req any) error {
	if err := c.Bind().Body(req); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return apperror.NewBadRequest("invalid JSON body")
		}
		return apperror.NewBadRequest("failed to parse request body")
	}
	return validator.ValidateStruct(req)
}

// paginationQuery binds page/per_page query params and normalizes them.
func paginationQuery(c fiber.Ctx) (page, perPage int, err error) {
	var q dto.PaginationQuery
	if err := c.Bind().Query(&q); err != nil {
		return 0, 0, err
	}
	page, perPage = pagination.Normalize(q.Page, q.PerPage)
	return page, perPage, nil
}
