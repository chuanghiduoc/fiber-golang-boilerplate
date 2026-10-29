package dto

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func TestCreateNotificationResponseSchemaAllowsNullNotification(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	ref := registry.Schema(reflect.TypeFor[CreateNotificationResponse](), true, "CreateNotificationResponse")
	if ref.Ref == "" {
		t.Fatal("CreateNotificationResponse must remain a component ref")
	}
	schema := registry.SchemaFromRef(ref.Ref)
	if schema == nil {
		t.Fatalf("component %q is not registered", ref.Ref)
	}

	notification := schema.Properties["notification"]
	if notification == nil {
		t.Fatal("notification property is missing")
	}
	if len(notification.AnyOf) != 2 {
		t.Fatalf("notification anyOf branches = %d, want object and null", len(notification.AnyOf))
	}
	if notification.AnyOf[0].Ref == "" {
		t.Fatalf("notification object branch must be a component ref, got %#v", notification.AnyOf[0])
	}
	if notification.AnyOf[1].Type != "null" {
		t.Fatalf("notification null branch type = %q, want null", notification.AnyOf[1].Type)
	}
	if registry.SchemaFromRef(notification.AnyOf[0].Ref) == nil {
		t.Fatalf("notification ref %q is not registered", notification.AnyOf[0].Ref)
	}

	raw, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal notification schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal notification schema: %v", err)
	}
	if _, ok := doc["anyOf"]; !ok {
		t.Fatalf("notification schema = %s, want anyOf", raw)
	}
}
