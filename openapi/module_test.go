package openapi_test

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/openapi"
	warrentest "github.com/MerseniBilel/warren/testing"
	"github.com/MerseniBilel/warren/transport"
	whttp "github.com/MerseniBilel/warren/transport/http"
	"github.com/MerseniBilel/warren/transport/http/servertest"
	"github.com/MerseniBilel/warren/validate"
)

type ctl struct{}

func (c *ctl) Register(r *transport.Registrar) {
	r.Post("/users", handler[registerUser, userView]{})
	r.Get("/users/{id}", handler[getUser, userView]{})
	// The escape hatch: no Req, no Res, nothing to derive a schema from.
	r.Raw(transport.ProtocolHTTP, "POST /uploads",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
}

func userModule() warren.Module {
	return warren.NewModule("user",
		warren.Providers(func() validate.Validator { return enforcing{} }),
		warren.Exports[validate.Validator](),
		warren.Controllers(func() *ctl { return &ctl{} }),
	)
}

// TestTheDocumentIsServedOverTheWire is the whole feature: a booted app, no
// annotations anywhere, and a client fetching a document that describes it.
func TestTheDocumentIsServedOverTheWire(t *testing.T) {
	t.Parallel()

	s := servertest.New(t, userModule(),
		servertest.With(warrentest.WithModules(
			openapi.Module(openapi.Title("Users"), openapi.Version("2.0.0")),
		)),
	)

	got := s.Get(t, "/openapi.json")
	if got.Status != http.StatusOK {
		t.Fatalf("GET /openapi.json = %s", got)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Undescribed []string `json:"x-warren-undescribed"`
		} `json:"paths"`
	}
	got.Decode(t, &doc)

	if doc.OpenAPI != "3.1.0" || doc.Info.Title != "Users" || doc.Info.Version != "2.0.0" {
		t.Errorf("info = %+v", doc)
	}
	for _, want := range []string{"/users", "/users/{id}", "/uploads"} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("the document omits %s — a client generated from it asserts the endpoint does not exist", want)
		}
	}
	// The raw route is LISTED and admits it is undescribed. Omitting it would
	// be the one error a generated client acts on.
	if u := doc.Paths["/uploads"]["post"].Undescribed; len(u) == 0 {
		t.Error("the raw route was emitted with no x-warren-undescribed — a client cannot tell it is unmodelled")
	}
	// And the document must NOT describe its own delivery mechanism.
	if _, ok := doc.Paths["/openapi.json"]; ok {
		t.Error("the document describes itself")
	}
}

// TestStrictTurnsRefusalsIntoABootFailure — off by default, and a gate a team
// turns on once its document is complete.
func TestStrictTurnsRefusalsIntoABootFailure(t *testing.T) {
	t.Parallel()

	// An HTTP adapter is in the graph because Table.Unserved() is checked
	// BEFORE this module's OnStart hook runs — without one the boot fails for
	// a different reason and this test would assert on the wrong refusal.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	a := warren.New(userModule(),
		whttp.Server(whttp.Listener(ln), whttp.DrainDelay(0)),
		openapi.Module(openapi.Strict()))
	err = a.Start(t.Context())
	if err == nil {
		_ = a.Stop(t.Context())
		t.Fatal("Strict() booted an app whose raw route cannot be described")
	}
	if !contains(err.Error(), "could not be fully described") {
		t.Errorf("unexpected boot error: %v", err)
	}
	t.Logf("refused, as Strict must:\n%v", err)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

var _ = json.Marshal
