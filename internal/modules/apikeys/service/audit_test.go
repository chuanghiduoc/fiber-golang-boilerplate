package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type recordingAudit struct {
	events []audit.Event
	err    error
}

func (r *recordingAudit) RecordTx(_ context.Context, event audit.Event) error {
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, event)
	return nil
}

func auditTestPrincipals() fakePrincipals {
	return fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
}

func TestService_LifecycleMutationsAreAudited(t *testing.T) {
	recorder := &recordingAudit{}
	svc := newAuditedTestService(newFakeRepo(), auditTestPrincipals(), recorder)
	ctx := testContext(1)

	created, err := svc.Create(ctx, 10, CreateInput{Name: "deploy", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := "renamed"
	if _, err := svc.Update(ctx, created.ID, UpdateInput{Name: &name}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rotated, err := svc.Rotate(ctx, 10, created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := svc.Revoke(ctx, rotated.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Idempotent re-revoke changes nothing and writes no second row.
	if err := svc.Revoke(ctx, rotated.ID); err != nil {
		t.Fatalf("re-revoke: %v", err)
	}

	want := []string{"api_key.created", "api_key.updated", "api_key.rotated", "api_key.revoked"}
	if len(recorder.events) != len(want) {
		t.Fatalf("events = %d, want %d", len(recorder.events), len(want))
	}
	for i, action := range want {
		event := recorder.events[i]
		if event.Action != action || event.Resource != "api_key" || event.ResourceID == "" {
			t.Fatalf("event %d = %+v, want %s on api_key", i, event, action)
		}
		for key, value := range event.Metadata {
			if s, ok := value.(string); ok && (s == created.Secret || s == rotated.Secret) {
				t.Fatalf("event %d metadata %q leaks the secret", i, key)
			}
		}
	}
}

func TestService_AuditFailureFailsClosed(t *testing.T) {
	recorder := &recordingAudit{err: errors.New("audit store down")}
	svc := newAuditedTestService(newFakeRepo(), auditTestPrincipals(), recorder)

	created, err := svc.Create(testContext(1), 10, CreateInput{Name: "deploy", Scopes: []string{"files:read"}})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusServiceUnavailable || appErr.Code != string(apperror.CodeSystemAuditUnavailable) {
		t.Fatalf("err = %v, want 503 system.audit_unavailable", err)
	}
	if created != nil {
		t.Fatal("a secret must not be returned when its audit row cannot be written")
	}
}
