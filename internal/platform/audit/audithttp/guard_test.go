package audithttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type completionCall struct {
	Intent     audit.IntentRef
	Completion audit.Completion
}

type guardRecorder struct {
	nextID      uuid.UUID
	intentErr   error
	completeErr error
	intents     []audit.Event
	completions []completionCall
}

func (r *guardRecorder) Record(context.Context, audit.Event) error { return nil }

func (r *guardRecorder) RecordTx(context.Context, pgx.Tx, audit.Event) error { return nil }

func (r *guardRecorder) RecordID(_ context.Context, event audit.Event) (audit.IntentRef, error) {
	r.intents = append(r.intents, event)
	if r.intentErr != nil {
		return audit.IntentRef{}, r.intentErr
	}
	r.nextID = uuid.New()
	return audit.IntentRef{ID: r.nextID, CreatedAt: time.Now().UTC()}, nil
}

func (r *guardRecorder) Complete(_ context.Context, intent audit.IntentRef, completion audit.Completion) error {
	r.completions = append(r.completions, completionCall{Intent: intent, Completion: completion})
	return r.completeErr
}

func newGuardApp(recorder audit.Recorder, policy Policy, handler fiber.Handler) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Post("/mutate/:id", Guard(recorder, policy), handler)
	return app
}

func doGuardRequest(t *testing.T, app *fiber.App) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/mutate/7", http.NoBody)
	require.NoError(t, err)
	return app.Test(req)
}

func TestGuard_RequiredIntentFailureFailsClosed(t *testing.T) {
	recorder := &guardRecorder{intentErr: errors.New("database down")}
	called := false
	app := newGuardApp(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}, func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.False(t, called, "the handler must not run without a durable intent row")
}

// Privileged mutations are fail-closed: best-effort must be opted into.
func TestGuard_ZeroModeFailsClosed(t *testing.T) {
	recorder := &guardRecorder{intentErr: errors.New("database down")}
	called := false
	app := newGuardApp(recorder, Policy{}, func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.False(t, called, "an empty Mode must default to ModeRequired")
}

func TestGuard_NoopRequiredFailsClosed(t *testing.T) {
	app := newGuardApp(audit.Noop{}, Policy{Mode: ModeRequired}, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

func TestGuard_BestEffortRunsHandlerAfterIntentFailure(t *testing.T) {
	recorder := &guardRecorder{intentErr: errors.New("database down")}
	called := false
	app := newGuardApp(recorder, Policy{Mode: ModeBestEffort, Action: "user.banned", Resource: "user"}, func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.True(t, called)
	assert.Empty(t, recorder.completions)
}

func TestGuard_CompletionMapping(t *testing.T) {
	tests := []struct {
		name      string
		handler   fiber.Handler
		want      string
		status    int
		errorCode string
	}{
		{
			name:    "success",
			handler: func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) },
			want:    audit.OutcomeSucceeded,
			status:  fiber.StatusNoContent,
		},
		{
			name: "denied",
			handler: func(fiber.Ctx) error {
				return apperror.NewCode(apperror.CodePermissionDenied, "no")
			},
			want:      audit.OutcomeDenied,
			status:    fiber.StatusForbidden,
			errorCode: string(apperror.CodePermissionDenied),
		},
		{
			name: "failed",
			handler: func(fiber.Ctx) error {
				return apperror.NewInternal("boom")
			},
			want:      audit.OutcomeFailed,
			status:    fiber.StatusInternalServerError,
			errorCode: "system.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &guardRecorder{}
			app := newGuardApp(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}, tt.handler)
			resp, err := doGuardRequest(t, app)
			require.NoError(t, err)
			assert.Equal(t, tt.status, resp.StatusCode)
			require.Len(t, recorder.intents, 1)
			assert.Equal(t, audit.OutcomeStarted, recorder.intents[0].Outcome)
			assert.Equal(t, audit.SourceHTTP, recorder.intents[0].Source)
			require.Len(t, recorder.completions, 1)
			assert.Equal(t, tt.want, recorder.completions[0].Completion.Outcome)
			assert.Equal(t, tt.status, recorder.completions[0].Completion.StatusCode)
			assert.Equal(t, tt.errorCode, recorder.completions[0].Completion.ErrorCode)
			assert.NotEqual(t, uuid.Nil, recorder.completions[0].Intent.ID)
		})
	}
}

func TestGuard_PanicCompletesFailedAndRepanics(t *testing.T) {
	recorder := &guardRecorder{}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fiber.NewError(fiber.StatusInternalServerError, "panic")
			}
		}()
		return c.Next()
	})
	app.Post("/mutate/:id", Guard(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}), func(fiber.Ctx) error {
		panic("handler exploded")
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Len(t, recorder.completions, 1)
	assert.Equal(t, audit.OutcomeFailed, recorder.completions[0].Completion.Outcome)
	assert.Equal(t, "panic", recorder.completions[0].Completion.ErrorCode)
}

func TestGuard_SkipsIdempotentReplay(t *testing.T) {
	recorder := &guardRecorder{}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		fiber.Locals[bool](c, "idempotency_replayed", true)
		return c.Next()
	})
	app.Post("/mutate/:id", Guard(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}), func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Empty(t, recorder.intents)
	assert.Empty(t, recorder.completions)
}

func TestGuard_RequireDomainCountsMissingEvent(t *testing.T) {
	before := counterValue(t, "audit_domain_missing_total")
	recorder := &guardRecorder{}
	app := newGuardApp(recorder, Policy{
		Mode:          ModeRequired,
		RequireDomain: true,
		Action:        "user.banned",
		Resource:      "user",
	}, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.Len(t, recorder.completions, 1)
	assert.Equal(t, audit.OutcomeUnknown, recorder.completions[0].Completion.Outcome)
	assert.Equal(t, "audit.domain_missing", recorder.completions[0].Completion.ErrorCode)
	assert.Equal(t, before+1, counterValue(t, "audit_domain_missing_total"))
}

func TestGuard_RequireDomainAcceptsServiceEvent(t *testing.T) {
	recorder := &guardRecorder{}
	app := newGuardApp(recorder, Policy{
		Mode:          ModeRequired,
		RequireDomain: true,
		Action:        "user.banned",
		Resource:      "user",
	}, func(c fiber.Ctx) error {
		_ = audit.NewServiceEvent(c.Context(), "user.banned", "user")
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.Len(t, recorder.completions, 1)
	assert.Equal(t, audit.OutcomeSucceeded, recorder.completions[0].Completion.Outcome)
}

func TestGuard_ResourceIDFallsBackToKeyParam(t *testing.T) {
	recorder := &guardRecorder{}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Put("/flags/:key", Guard(recorder, Policy{Mode: ModeRequired, Action: "feature_flag.updated", Resource: "feature_flag"}),
		func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusForbidden) })

	req, err := http.NewRequest(http.MethodPut, "/flags/user_registration", http.NoBody)
	require.NoError(t, err)
	_, err = app.Test(req)
	require.NoError(t, err)
	require.Len(t, recorder.intents, 1)
	assert.Equal(t, "user_registration", recorder.intents[0].ResourceID)
}

func TestGuard_HashesIdempotencyKey(t *testing.T) {
	recorder := &guardRecorder{}
	app := newGuardApp(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	req, _ := http.NewRequest(http.MethodPost, "/mutate/7", http.NoBody)
	req.Header.Set("Idempotency-Key", "secret-client-key")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.Len(t, recorder.intents, 1)
	assert.NotEqual(t, "secret-client-key", recorder.intents[0].IdempotencyKeyHash)
	assert.Equal(t, audit.HashIdempotencyKey("secret-client-key"), recorder.intents[0].IdempotencyKeyHash)
}

func TestGuard_CompletionFailurePreservesResponse(t *testing.T) {
	recorder := &guardRecorder{completeErr: audit.ErrAlreadyCompleted}
	app := newGuardApp(recorder, Policy{Mode: ModeRequired, Action: "user.banned", Resource: "user"}, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := doGuardRequest(t, app)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode,
		"a completion failure cannot roll back a committed Tier 2 mutation")
}

// testID returns a deterministic UUID for a small fixture number.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
