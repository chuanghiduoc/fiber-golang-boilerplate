package privacy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

type fakeProvider struct{ name string }

func (f fakeProvider) Export(context.Context, uuid.UUID, uuid.UUID) (Section, error) {
	return Section{Name: f.name, Data: json.RawMessage(`{}`)}, nil
}

func (f fakeProvider) Erase(context.Context, uuid.UUID, uuid.UUID) error { return nil }

func TestRegistryKeepsRegistrationOrderAndSnapshots(t *testing.T) {
	r := NewRegistry()
	r.Register(Provider{Name: "identity", Exporter: fakeProvider{"identity"}, Eraser: fakeProvider{"identity"}})
	r.Register(Provider{Name: "files", Exporter: fakeProvider{"files"}, Eraser: fakeProvider{"files"}})

	got := r.Providers()
	if len(got) != 2 || got[0].Name != "identity" || got[1].Name != "files" {
		t.Fatalf("providers = %+v, want identity then files", got)
	}
	got[0].Name = "mutated"
	if r.Providers()[0].Name != "identity" {
		t.Fatal("Providers must return a snapshot")
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	r := NewRegistry()
	p := Provider{Name: "identity", Exporter: fakeProvider{"identity"}, Eraser: fakeProvider{"identity"}}
	r.Register(p)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic at startup")
		}
	}()
	r.Register(p)
}

func TestRegistryRequiresExporterAndEraser(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("incomplete provider must panic")
		}
	}()
	r.Register(Provider{Name: "files", Exporter: fakeProvider{"files"}})
}

func TestBudgetUsesCamelCaseJSONFields(t *testing.T) {
	raw, err := json.Marshal(Budget{RowLimit: 10, ByteLimit: 20, Returned: 3, Truncated: true})
	if err != nil {
		t.Fatalf("marshal budget: %v", err)
	}
	want := `{"rowLimit":10,"byteLimit":20,"returned":3,"truncated":true}`
	if string(raw) != want {
		t.Fatalf("budget json = %s, want %s", raw, want)
	}
}
