package openapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"reflect"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/openapi"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

type registerUser struct {
	Email string `json:"email" validate:"required,email"`
	Name  string `json:"name"  validate:"required,min=2,max=64"`
	Role  string `json:"role"  validate:"oneof=admin member"`
}

type getUser struct {
	ID string `param:"id" validate:"required"`
}

type listUsers struct {
	Limit  int    `query:"limit"  validate:"min=1,max=100"`
	Status string `query:"status"`
}

type userView struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type handler[Req, Res any] struct{}

func (handler[Req, Res]) Handle(context.Context, Req) (Res, error) { var z Res; return z, nil }

func table(t *testing.T, v validate.Validator) *transport.Table {
	t.Helper()
	// validate.None() accepts every tag and enforces NOTHING. Core's default
	// validator refuses email/min/oneof rather than under-validating, and
	// installing validate/playground here would put a second require in
	// openapi/go.mod, which its definition of done forbids.
	//
	// It is also the exact configuration TestEmitRefusesUnderValidateNone
	// exists for: under None() the tags are decoration, so a document that
	// published them as constraints would be a lie the framework generated.
	b := transport.NewBuilder(transport.WithValidator(v))
	r := b.For("user")
	r.Post("/users", handler[registerUser, userView]{})
	r.Get("/users/{id}", handler[getUser, userView]{})
	r.Get("/users", handler[listUsers, userView]{})
	if err := b.Failures(); err != nil {
		t.Fatalf("registration: %v", err)
	}
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	return tbl
}

func TestEmitDerivesTheDocumentFromTagsAlone(t *testing.T) {
	t.Parallel()

	doc, err := openapi.Emit(table(t, enforcing{}), openapi.Title("Users"), openapi.Version("1.2.0"))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	b, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	got := string(b)
	t.Log(got)

	for _, want := range []string{
		`"openapi": "3.1.0"`,
		`"title": "Users"`,
		`"/users"`,
		`"/users/{id}"`,
		`"format": "email"`, // validate:"email"
		`"minLength": 2`,    // validate:"min=2" on a string
		`"maxLength": 64`,   // validate:"max=64"
		`"admin"`,           // validate:"oneof=..."
		`"in": "path"`,      // param: tag
		`"in": "query"`,     // query: tag
		`"warren.Error"`,    // the error envelope
	} {
		if !strings.Contains(got, want) {
			t.Errorf("document does not contain %s", want)
		}
	}

	// A query int must get a NUMERIC bound, not a length one.
	if strings.Contains(got, `"minLength": 1`) {
		t.Error("min=1 on an int field became minLength — a client generator would enforce a string rule on a number")
	}

	// The document must be valid JSON, which is also what makes it valid
	// YAML 1.2 — the reason no YAML dependency is bought.
	var any map[string]any
	if err := json.Unmarshal(b, &any); err != nil {
		t.Fatalf("the emitted document is not valid JSON: %v", err)
	}
}

func TestEmitIsDeterministic(t *testing.T) {
	t.Parallel()

	tbl := table(t, enforcing{})
	first, err := openapi.Emit(tbl)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	// Map iteration order is randomised per run in Go, so a document built
	// from maps reorders unless every walk is sorted. A document that
	// reorders makes every diff unreadable and every checked-in copy churn.
	for range 100 {
		doc, err := openapi.Emit(tbl)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		got, err := doc.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatal("the document is not deterministic across runs")
		}
	}
}

var _ = app.Handler[registerUser, userView](handler[registerUser, userView]{})

// enforcing is a validator that ACCEPTS every tag and reports that it enforces
// them. Core's own validate.Required() refuses email/min/oneof rather than
// under-validating, and installing validate/playground here would put a second
// require in openapi/go.mod, which its definition of done forbids.
//
// What matters to the emitter is only that this is NOT validate.None(): the
// document's constraints are promises, and they may be published exactly when
// something checks them.
type enforcing struct{}

func (enforcing) Plan(reflect.Type) (validate.Rule, error) {
	return func(any) error { return nil }, nil
}

// TestEmitOmitsConstraintsUnderValidateNone is the trap, and it is the reason
// transport.Table gained a Validator accessor.
//
// Under validate.None() every tag is accepted and NOTHING is checked. A
// document that published `required`, `minLength` and `format` from those tags
// would assert guarantees the service does not make — and a generated client
// would then reject requests the server accepts. The SHAPE stays true; only
// the promises go.
func TestEmitOmitsConstraintsUnderValidateNone(t *testing.T) {
	t.Parallel()

	doc, err := openapi.Emit(table(t, validate.None()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	b, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// Scoped to the DERIVED component. requestBody.required ("you must send a
	// body") and the hardcoded error envelope are true whatever the validator
	// does; what must not appear is a constraint read off a `validate:` tag.
	var doc2 struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &doc2); err != nil {
		t.Fatal(err)
	}
	derived := string(doc2.Components.Schemas["openapi_test.registerUser"])
	if derived == "" {
		t.Fatalf("the request type was not described at all:\n%s", got)
	}
	for _, promise := range []string{`"format"`, `"minLength"`, `"maxLength"`, `"required"`, `"enum"`} {
		if strings.Contains(derived, promise) {
			t.Errorf("the derived schema published %s while the validator enforces nothing:\n%s", promise, derived)
		}
	}
	// The shape must survive: a client still needs the paths and the types.
	for _, want := range []string{`"/users/{id}"`, `"type": "string"`, `"in": "path"`} {
		if !strings.Contains(got, want) {
			t.Errorf("dropping the constraints also dropped %s — only the promises should go", want)
		}
	}
	// And it says so, rather than quietly under-describing.
	var said bool
	for _, r := range doc.Refusals() {
		if strings.Contains(r.Reason, "validate.None") {
			said = true
		}
	}
	if !said {
		t.Error("no refusal explains why the constraints are missing")
	}
}
