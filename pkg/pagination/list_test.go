package pagination_test

import (
	"encoding/json"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func TestListEnvelope(t *testing.T) {
	list := pagination.NewList[string]("", nil, false, "ignored")
	if list.Object != "list" || list.URL != "" || list.Data == nil || list.HasMore || list.NextCursor != "" {
		t.Fatalf("list = %+v", list)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"object":"list","url":"","data":[],"hasMore":false}` {
		t.Fatalf("empty list JSON = %s", raw)
	}

	list = pagination.NewList("/api/v1/files", []string{"a"}, true, "cursor")
	if !list.HasMore || list.NextCursor != "cursor" {
		t.Fatalf("list = %+v", list)
	}
}
