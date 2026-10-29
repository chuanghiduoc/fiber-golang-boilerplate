package apikeydto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The create contract must not offer an owner: a key is always owned by the
// authenticated caller, so an administrator cannot mint one for another member
// and borrow that member's /me routes and audit actor.
func TestCreateRequestHasNoOwnerField(t *testing.T) {
	typ := reflect.TypeOf(CreateAPIKeyRequest{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		switch jsonName {
		case "userId", "user_id", "ownerId", "owner_id":
			t.Fatalf("CreateRequest.%s exposes an owner field (%q)", field.Name, jsonName)
		}
	}
}

// TestAPIKeyResponseUsesUUIDStrings pins the wire contract: ids are UUID
// strings and expiresAt is always present because every key expires.
func TestAPIKeyResponseUsesUUIDStrings(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000005")
	userID := uuid.MustParse("0192c9b0-7e4a-7000-8000-00000000000a")
	expiresAt := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	raw, err := json.Marshal(APIKeyResponse{ID: id, UserID: userID, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded["id"]; got != id.String() {
		t.Fatalf("id = %v, want the UUID string %q", got, id)
	}
	if got := decoded["userId"]; got != userID.String() {
		t.Fatalf("userId = %v, want the UUID string %q", got, userID)
	}
	if _, ok := decoded["expiresAt"]; !ok {
		t.Fatal("expiresAt must always be serialized: every key expires")
	}
}

func TestUpdateAPIKeySchemaConstrainsNameAndScopes(t *testing.T) {
	nameSchema := (OptionalString{}).Schema(nil)
	if nameSchema.MinLength == nil || *nameSchema.MinLength != 1 {
		t.Fatalf("name MinLength = %v, want 1", nameSchema.MinLength)
	}
	if nameSchema.MaxLength == nil || *nameSchema.MaxLength != 255 {
		t.Fatalf("name MaxLength = %v, want 255", nameSchema.MaxLength)
	}
	if !nameSchema.Nullable {
		t.Fatal("name schema must remain nullable for merge-patch decoding")
	}

	scopesSchema := (OptionalStrings{}).Schema(nil)
	if scopesSchema.MinItems == nil || *scopesSchema.MinItems != 1 {
		t.Fatalf("scopes MinItems = %v, want 1", scopesSchema.MinItems)
	}
	if scopesSchema.MaxItems == nil || *scopesSchema.MaxItems != 100 {
		t.Fatalf("scopes MaxItems = %v, want 100", scopesSchema.MaxItems)
	}
	if !scopesSchema.Nullable {
		t.Fatal("scopes schema must remain nullable for merge-patch decoding")
	}
}
