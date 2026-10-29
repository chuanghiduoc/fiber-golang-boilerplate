package dto

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

// Optional is one member of an RFC 7396 merge-patch request body. It records
// presence and explicit null while documenting the member's own schema rather
// than the presence envelope.
type Optional[T any] patch.Field[T]

// UnmarshalJSON delegates presence/null tracking to patch.Field.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	return (*patch.Field[T])(o).UnmarshalJSON(data)
}

// Schema documents the member as T, nullable for an explicit clear.
func (Optional[T]) Schema(r huma.Registry) *huma.Schema {
	schema := huma.SchemaFromType(r, reflect.TypeFor[T]())
	schema.Nullable = true
	return schema
}

// RoleIDList is a UUID list whose wire schema is an array of UUID strings, not
// the [16]byte array huma would infer from uuid.UUID.
type RoleIDList []uuid.UUID

// Schema implements huma.SchemaProvider.
func (RoleIDList) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type:  huma.TypeArray,
		Items: &huma.Schema{Type: huma.TypeString, Format: "uuid"},
	}
}
