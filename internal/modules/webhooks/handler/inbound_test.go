package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestCreateInboundSourceCreatedLocationAndSecret(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000030")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var got webhookservice.CreateInboundSourceInput
	svc := &fakeService{createInboundSource: func(_ context.Context, actorID uuid.UUID, in webhookservice.CreateInboundSourceInput) (*webhookservice.InboundSource, string, error) {
		assert.Equal(t, testActorID, actorID)
		got = in
		return sourceFixture(id, now), "whsec_source", nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound-sources", map[string]any{
		"name": "Stripe", "slug": "stripe", "status": "disabled",
	}, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "body: %s", body)
	assert.Equal(t, "/api/v1/webhooks/inbound-sources/"+id.String(), resp.Header.Get("Location"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, webhookservice.CreateInboundSourceInput{Name: "Stripe", Slug: "stripe", Status: "disabled"}, got)

	created := decode[dto.InboundSourceSecretResponse](t, body)
	assert.Equal(t, id, created.ID)
	assert.Equal(t, "/api/v1/webhooks/inbound/"+id.String(), created.ReceiverURL)
	assert.Equal(t, "whsec_source", created.Secret)
}

func TestListInboundSourcesEnvelope(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000031")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := &fakeService{listInboundSources: func(_ context.Context, limit int, _ string) ([]webhookservice.InboundSource, bool, error) {
		assert.Equal(t, 20, limit, "the documented default page size")
		return []webhookservice.InboundSource{*sourceFixture(id, now)}, false, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/inbound-sources", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	page := decode[dtoList[dto.InboundSourceResponse]](t, body)
	require.Len(t, page.Data, 1)
	assert.Equal(t, "list", page.Object)
	assert.Equal(t, "/api/v1/webhooks/inbound-sources", page.URL)
	assert.False(t, page.HasMore)
	assert.Empty(t, page.NextCursor)
}

func TestUpdateInboundSourceMergePatchAndETag(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000032")
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ifMatch := strconv.Quote(strconv.FormatInt(updated.UnixNano(), 10))
	wantETag := strconv.Quote(strconv.FormatInt(updated.Add(time.Minute).UnixNano(), 10))

	current := sourceFixture(id, updated)
	var patch webhookservice.UpdateInboundSourceInput
	svc := &fakeService{
		getInboundSource: func(context.Context, uuid.UUID) (*webhookservice.InboundSource, error) {
			return current, nil
		},
		updateInboundSource: func(_ context.Context, actorID, got uuid.UUID, in webhookservice.UpdateInboundSourceInput) (*webhookservice.InboundSource, error) {
			assert.Equal(t, testActorID, actorID)
			assert.Equal(t, id, got)
			patch = in
			next := sourceFixture(id, updated.Add(time.Minute))
			next.Name, next.Status = in.Name, in.Status
			return next, nil
		},
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/inbound-sources/"+id.String(), map[string]any{
		"name": "Payments", "status": "disabled",
	}, map[string]string{
		"If-Match":     ifMatch,
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, wantETag, resp.Header.Get("ETag"))
	expectedTime := updated
	assert.Equal(t, webhookservice.UpdateInboundSourceInput{Name: "Payments", Slug: "stripe", Status: "disabled", ExpectedUpdatedAt: &expectedTime}, patch)

	// A stale validator is rejected before the mutation.
	svc.getInboundSource = func(context.Context, uuid.UUID) (*webhookservice.InboundSource, error) {
		return current, nil
	}
	svc.updateInboundSource = nil
	resp, body = do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/inbound-sources/"+id.String(), map[string]any{"name": "Nope"}, map[string]string{
		"If-Match":     `"stale"`,
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, "body: %s", body)
	assert.Equal(t, string(apperror.CodeResourcePreconditionFailed), decode[apperror.ProblemDetails](t, body).Code)
}

func TestRotateInboundSourceSecretReturnsOnlySecret(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000033")
	var gotExpire bool
	svc := &fakeService{rotateInboundSecret: func(_ context.Context, actorID, got uuid.UUID, expirePrevious bool) (*webhookservice.InboundSource, string, error) {
		assert.Equal(t, testActorID, actorID)
		assert.Equal(t, id, got)
		gotExpire = expirePrevious
		return sourceFixture(id, time.Now()), "whsec_inbound_rotated", nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound-sources/"+id.String()+"/rotate-secret", map[string]any{
		"expirePrevious": true,
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.True(t, gotExpire)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.Equal(t, map[string]any{"secret": "whsec_inbound_rotated"}, raw)
}

func TestDeleteInboundSourceNoContent(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000034")
	called := false
	svc := &fakeService{deleteInboundSource: func(_ context.Context, actorID, got uuid.UUID) error {
		called = true
		assert.Equal(t, testActorID, actorID)
		assert.Equal(t, id, got)
		return nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodDelete, "/api/v1/webhooks/inbound-sources/"+id.String(), nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "body: %s", body)
	assert.Empty(t, body)
	assert.True(t, called)
}

func TestListInboundSourceEventsEnvelope(t *testing.T) {
	sourceID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000035")
	eventID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000036")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var gotLimit int
	svc := &fakeService{listInboundEvents: func(_ context.Context, got uuid.UUID, limit int, startingAfter string) ([]webhookservice.InboundEvent, bool, error) {
		assert.Equal(t, sourceID, got)
		gotLimit = limit
		assert.Equal(t, "cursor", startingAfter)
		return []webhookservice.InboundEvent{{
			ID: eventID, TenantID: tenantID(), SourceID: sourceID,
			ExternalEventID: "msg_1", EventType: "payment.succeeded",
			Payload: json.RawMessage(`{"amount":10}`), ReceivedAt: now,
		}}, true, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	path := "/api/v1/webhooks/inbound-sources/" + sourceID.String() + "/events?limit=7&startingAfter=cursor"
	resp, body := do(t, fiberApp, http.MethodGet, path, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, 7, gotLimit)

	page := decode[dtoList[dto.InboundEventResponse]](t, body)
	require.Len(t, page.Data, 1)
	assert.Equal(t, "msg_1", page.Data[0].ExternalEventID)
	assert.Equal(t, "/api/v1/webhooks/inbound-sources/"+sourceID.String()+"/events", page.URL)
	assert.True(t, page.HasMore)
	assert.NotEmpty(t, page.NextCursor)
}

func TestReceiveInboundWebhookNewAndDuplicate(t *testing.T) {
	sourceID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000037")
	eventID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000038")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	duplicate := false
	var gotHeaders http.Header
	var gotBody []byte
	svc := &fakeService{receiveInbound: func(_ context.Context, got uuid.UUID, headers http.Header, body []byte) (*webhookservice.InboundReceipt, error) {
		assert.Equal(t, sourceID, got)
		gotHeaders = headers.Clone()
		gotBody = append([]byte(nil), body...)
		return &webhookservice.InboundReceipt{
			ID: eventID, TenantID: tenantID(), SourceID: sourceID,
			EventID: "msg_1", EventType: "payment.succeeded", ReceivedAt: now, Duplicate: duplicate,
		}, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	headers := map[string]string{
		"webhook-id":        "msg_1",
		"webhook-timestamp": "1700000000",
		"webhook-signature": "v1,abc",
	}
	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound/"+sourceID.String(), map[string]any{
		"type": "payment.succeeded", "data": map[string]any{"amount": 10},
	}, headers)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "body: %s", body)
	assert.Equal(t, "msg_1", gotHeaders.Get("webhook-id"))
	assert.Equal(t, "1700000000", gotHeaders.Get("webhook-timestamp"))
	assert.Equal(t, "v1,abc", gotHeaders.Get("webhook-signature"))
	assert.JSONEq(t, `{"type":"payment.succeeded","data":{"amount":10}}`, string(gotBody))
	assert.False(t, decode[dto.InboundReceipt](t, body).Duplicate)

	duplicate = true
	resp, body = do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound/"+sourceID.String(), map[string]any{
		"type": "payment.succeeded",
	}, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.True(t, decode[dto.InboundReceipt](t, body).Duplicate)
}

func TestReceiveInboundWebhookRequiresStandardHeaders(t *testing.T) {
	sourceID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000039")
	fiberApp, _ := newAPI(t, &fakeService{})

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound/"+sourceID.String(), map[string]any{
		"type": "payment.succeeded",
	}, map[string]string{"webhook-id": "msg_1"})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "body: %s", body)
	problem := decode[apperror.ProblemDetails](t, body)
	assert.Equal(t, string(apperror.CodeRequestValidationFailed), problem.Code)
	paths := make([]string, 0, len(problem.Errors))
	for _, field := range problem.Errors {
		paths = append(paths, field.Path)
	}
	assert.Contains(t, paths, "header.webhook-timestamp")
	assert.Contains(t, paths, "header.webhook-signature")
}

func TestReceiveInboundWebhookSignatureFailureIsProblem(t *testing.T) {
	sourceID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000040")
	svc := &fakeService{receiveInbound: func(context.Context, uuid.UUID, http.Header, []byte) (*webhookservice.InboundReceipt, error) {
		return nil, apperror.NewUnauthorized("webhook signature is invalid")
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/inbound/"+sourceID.String(), map[string]any{"type": "x"}, map[string]string{
		"webhook-id": "msg_1", "webhook-timestamp": "1700000000", "webhook-signature": "v1,abc",
	})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "body: %s", body)
	problem := decode[apperror.ProblemDetails](t, body)
	assert.Equal(t, string(apperror.CodeAuthUnauthenticated), problem.Code)
	assert.Equal(t, "webhook signature is invalid", problem.Detail)
}
