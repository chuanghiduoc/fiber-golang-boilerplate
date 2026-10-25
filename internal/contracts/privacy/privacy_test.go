package privacy

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeProvider struct{ name string }

func (f fakeProvider) Export(context.Context, int64, int64) (Section, error) {
	return Section{Name: f.name, Data: json.RawMessage(`{}`)}, nil
}

func (f fakeProvider) Erase(context.Context, int64, int64) error { return nil }

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
