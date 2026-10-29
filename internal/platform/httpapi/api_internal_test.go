package httpapi

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func TestPrepareRecordsOperationsAndOrderAtomically(t *testing.T) {
	api := &API{
		deps:         Deps{},
		maxBodyBytes: DefaultMaxBodyBytes,
		orders:       map[string][]string{},
		catalogValue: nil,
	}
	const operations = 32
	var wg sync.WaitGroup
	for i := 0; i < operations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("probe%d", i)
			api.prepare(Op{
				Method:         http.MethodGet,
				Path:           fmt.Sprintf("/probe-%d", i),
				OperationID:    id,
				Tags:           []string{"test"},
				Summary:        "Concurrent probe",
				Infrastructure: true,
			}, Policy{Public: true}, reflect.TypeFor[struct{}]())
		}(i)
	}
	wg.Wait()

	if got := len(api.Operations()); got != operations {
		t.Fatalf("recorded operations = %d, want %d", got, operations)
	}
	for i := 0; i < operations; i++ {
		id := fmt.Sprintf("probe%d", i)
		if _, ok := api.MiddlewareOrder(id); !ok {
			t.Fatalf("middleware order missing for %s", id)
		}
	}
}

func TestNewRejectsNegativeBodyReadTimeout(t *testing.T) {
	app := fiber.New()
	pass := func(c fiber.Ctx) error { return c.Next() }
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a negative BodyReadTimeout")
		}
	}()
	New(app, app, DefaultConfig("Test", "1.0.0", "test"), Deps{
		Authenticate:    pass,
		AuthenticateJWT: pass,
		BodyReadTimeout: -time.Second,
	})
}
