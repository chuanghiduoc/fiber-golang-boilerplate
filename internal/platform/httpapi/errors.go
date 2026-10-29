package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// errorRenderer is the per-request catalog/i18n pair. huma's error
// constructors are package-global, so the hook installed below reads this from
// the request context instead of closing over one API's dependencies.
type errorRenderer struct {
	catalog *apperror.Catalog
	bundle  *i18n.Bundle
}

type errorRendererKey struct{}

func withErrorRenderer(ctx context.Context, renderer *errorRenderer) context.Context {
	return context.WithValue(ctx, errorRendererKey{}, renderer)
}

func errorRendererFrom(ctx context.Context) *errorRenderer {
	renderer, _ := ctx.Value(errorRendererKey{}).(*errorRenderer)
	return renderer
}

var installErrorHooksOnce sync.Once

// installErrorHooks installs huma's global constructors exactly once. Each API
// injects its own renderer through the operation middleware, so a second API in
// the same process (tests) cannot replace the first API's catalog or locale.
func installErrorHooks() {
	installErrorHooksOnce.Do(func() {
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			appErr := apperror.NewForStatus(status, msg)
			problem := apperror.PlatformCatalog().Problem(appErr, "", "")
			problem.Errors = humaFieldErrors(errs)
			return &problem
		}
		huma.NewErrorWithContext = func(ctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
			catalog := apperror.PlatformCatalog()
			var bundle *i18n.Bundle
			if renderer := errorRendererFrom(ctx.Context()); renderer != nil {
				if renderer.catalog != nil {
					catalog = renderer.catalog
				}
				bundle = renderer.bundle
			}
			appErr := apperror.NewForStatus(status, msg)
			problem := catalog.Problem(appErr, requestctx.RequestID(ctx.Context()), ctx.URL().Path)
			problem.Errors = localizeFieldErrorsWith(bundle, i18n.LocaleFromContext(ctx.Context()), humaFieldErrors(errs))
			if status >= http.StatusInternalServerError {
				slog.ErrorContext(ctx.Context(), "huma request failed",
					slog.String("path", ctx.URL().Path),
					slog.String("request_id", requestctx.RequestID(ctx.Context())),
					slog.Any("cause", errors.Join(errs...)),
				)
			}
			return &problem
		}
	})
}

// errorFor converts anything a handler returns into the one RFC 9457 shape.
// Internal causes are logged for 5xx and never copied into the response.
func (a *API) errorFor(ctx context.Context, err error) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return a.buildProblem(ctx, requestctx.Path(ctx), appErr)
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return a.buildProblem(ctx, requestctx.Path(ctx), apperror.NewCode(apperror.CodeSystemTimeout, "request timed out"))
	case errors.Is(err, context.Canceled):
		return a.buildProblem(ctx, requestctx.Path(ctx), apperror.NewForStatus(apperror.StatusClientClosedRequest, "request canceled by client"))
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return a.buildProblem(ctx, requestctx.Path(ctx), apperror.NewForStatus(fiberErr.Code, fiberErr.Message))
	}

	// huma's own helpers (huma.Error400BadRequest, conditional 304/412, etc.)
	// return a StatusError. Keep its status instead of turning it into a 500.
	var problem *apperror.ProblemDetails
	if errors.As(err, &problem) {
		return problem
	}
	var statusErr huma.StatusError
	if errors.As(err, &statusErr) {
		return a.buildProblem(ctx, requestctx.Path(ctx), apperror.NewForStatus(statusErr.GetStatus(), statusErr.Error()))
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return a.buildProblem(ctx, requestctx.Path(ctx), apperror.NewNotFound("resource not found"))
	}

	return a.buildProblem(ctx, requestctx.Path(ctx), apperror.Internal(err, "Internal Server Error"))
}

func (a *API) buildProblem(ctx context.Context, instance string, appErr *apperror.AppError) *apperror.ProblemDetails {
	problem := a.catalog().Problem(appErr, requestctx.RequestID(ctx), instance)
	if problem.Status >= http.StatusInternalServerError {
		cause := appErr.Cause
		if cause == nil {
			cause = appErr
		}
		slog.ErrorContext(ctx, "request failed with an internal error",
			slog.String("path", instance),
			slog.String("request_id", requestctx.RequestID(ctx)),
			slog.String("code", problem.Code),
			slog.Any("cause", cause),
		)
	}
	if bundle := a.deps.I18n; bundle != nil {
		locale := i18n.LocaleFromContext(ctx)
		if title := bundle.Translate(locale, "errors."+problem.Code+".title", nil); title != "" && title != "errors."+problem.Code+".title" {
			problem.Title = title
		}
		if appErr.DetailKey != "" {
			if detail := bundle.Translate(locale, appErr.DetailKey, appErr.DetailParams); detail != "" && detail != appErr.DetailKey {
				problem.Detail = detail
			}
		}
	}
	problem.Errors = localizeFieldErrorsWith(a.deps.I18n, i18n.LocaleFromContext(ctx), problem.Errors)
	return &problem
}

func localizeFieldErrorsWith(bundle *i18n.Bundle, locale string, fields []apperror.FieldError) []apperror.FieldError {
	if bundle == nil || len(fields) == 0 {
		return fields
	}
	out := make([]apperror.FieldError, len(fields))
	for i, field := range fields {
		params := make(map[string]any, len(field.Params)+1)
		params["path"] = field.Path
		for key, value := range field.Params {
			params[key] = value
		}
		key := "validation." + field.Code
		if message := bundle.Translate(locale, key, params); message != "" && message != key {
			field.Message = message
		}
		out[i] = field
	}
	return out
}

// humaFieldErrors flattens huma's validation details into the flat errors[]
// list every other error response uses.
func humaFieldErrors(errs []error) []apperror.FieldError {
	out := make([]apperror.FieldError, 0, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		field := apperror.FieldError{Code: "invalid", Message: err.Error()}
		var detail huma.ErrorDetailer
		if errors.As(err, &detail) {
			d := detail.ErrorDetail()
			field.Path = d.Location
			field.Message = d.Message
		}
		out = append(out, field)
	}
	return out
}
