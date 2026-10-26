package apikeydto

import (
	"reflect"
	"strings"
	"testing"
)

// The create contract must not offer an owner: a key is always owned by the
// authenticated caller, so an administrator cannot mint one for another member
// and borrow that member's /me routes and audit actor.
func TestCreateRequestHasNoOwnerField(t *testing.T) {
	typ := reflect.TypeOf(CreateRequest{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		switch jsonName {
		case "userId", "user_id", "ownerId", "owner_id":
			t.Fatalf("CreateRequest.%s exposes an owner field (%q)", field.Name, jsonName)
		}
	}
}
