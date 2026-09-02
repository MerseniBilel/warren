package openapi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/openapi"
	"github.com/MerseniBilel/warren/transport"

	catalog "github.com/MerseniBilel/warren/openapi/internal/fixture/catalog/application"
	lending "github.com/MerseniBilel/warren/openapi/internal/fixture/lending/application"
	other "github.com/MerseniBilel/warren/openapi/internal/other/lending/application"
)

// emit builds a document over the routes fn registers, with an enforcing
// validator so constraints are published rather than dropped.
func emit(t *testing.T, fn func(r *transport.Registrar), opts ...openapi.Option) (openapi.Document, string) {
	t.Helper()

	b := transport.NewBuilder(transport.WithValidator(enforcing{}))
	fn(b.For("fixture"))
	if err := b.Failures(); err != nil {
		t.Fatalf("registration: %v", err)
	}
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	doc, err := openapi.Emit(tbl, opts...)
	if err != nil && len(opts) == 0 {
		t.Fatalf("Emit: %v", err)
	}
	raw, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	return doc, string(raw)
}

// TestTwoSameNamedTypesGetTwoSchemas is the regression for field test #14's
// first finding.
//
// Before the fix the key was the LAST path element plus the type name, so both
// BookViews keyed on "application.BookView", the second was never walked, and
// both routes referenced the first — publishing five fields that do not exist
// for GET /books/{id}/loans and none of the three that do. Nothing refused,
// because a wrong answer was given confidently rather than declined.
func TestTwoSameNamedTypesGetTwoSchemas(t *testing.T) {
	t.Parallel()

	_, got := emit(t, func(r *transport.Registrar) {
		r.Post("/books", handler[catalog.BookView, catalog.BookView]{})
		r.Post("/loans", handler[lending.BookView, lending.BookView]{})
	})
	t.Log(got)

	for _, want := range []string{
		`"catalog.application.BookView"`,
		`"lending.application.BookView"`,
		`"copies_available"`, // catalog's, which used to be the only survivor
		`"open_loan_ids"`,    // lending's, which used to vanish
	} {
		if !strings.Contains(got, want) {
			t.Errorf("document does not contain %s:\n%s", want, got)
		}
	}
	// The defect itself: the unqualified key must not appear at all.
	if strings.Contains(got, `"application.BookView"`) {
		t.Errorf("the ambiguous key survived — two types are sharing one schema:\n%s", got)
	}
}

// TestThreeWayCollisionResolvesAtTheShortestDepth checks that qualification is
// added only as far as it is needed, and PER TYPE.
//
// catalog is unique after one extra segment; the two lendings are not, and
// need a second. A scheme that qualified everything to the same depth would
// pass a two-way test and fail this one.
func TestThreeWayCollisionResolvesAtTheShortestDepth(t *testing.T) {
	t.Parallel()

	_, got := emit(t, func(r *transport.Registrar) {
		r.Post("/a", handler[catalog.BookView, catalog.BookView]{})
		r.Post("/b", handler[lending.BookView, lending.BookView]{})
		r.Post("/c", handler[other.BookView, other.BookView]{})
	})
	t.Log(got)

	for _, want := range []string{
		`"catalog.application.BookView"`,         // unique at one segment
		`"fixture.lending.application.BookView"`, // needed two
		`"other.lending.application.BookView"`,   // needed two
	} {
		if !strings.Contains(got, want) {
			t.Errorf("document does not contain %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"lending.application.BookView"`) {
		t.Errorf("a key that is ambiguous between two lendings was published:\n%s", got)
	}
}

// TestOneTypeOnTwoRoutesStaysOneSchema is the control on the two above: the
// dedup the component mechanism exists for must survive the naming change.
func TestOneTypeOnTwoRoutesStaysOneSchema(t *testing.T) {
	t.Parallel()

	_, got := emit(t, func(r *transport.Registrar) {
		r.Post("/x", handler[catalog.BookView, catalog.BookView]{})
		r.Post("/y", handler[catalog.BookView, catalog.BookView]{})
	})
	t.Log(got)

	if n := strings.Count(got, `"application.BookView": {`); n != 1 {
		t.Errorf("one type on two routes produced %d schemas, want 1:\n%s", n, got)
	}
	// And with no collision the key stays SHORT — qualification is added only
	// where something needs telling apart, which is what keeps the common
	// document readable. The two tests above are the same type at greater
	// depth; this one is the floor.
	if strings.Contains(got, `"catalog.application.BookView"`) {
		t.Errorf("an uncontested type was qualified anyway:\n%s", got)
	}
}

// --- the wire-form ladder ------------------------------------------------

type textual struct{}

func (textual) MarshalText() ([]byte, error) { return []byte("t"), nil }

type opaque struct{}

func (opaque) MarshalJSON() ([]byte, error) { return []byte(`{"raw":1}`), nil }

type timings struct {
	At    time.Time     `json:"at"`
	Every time.Duration `json:"every"`
	Tag   textual       `json:"tag"`
}

type opaqueHolder struct {
	Thing opaque `json:"thing"`
}

// TestTimeIsAStringNotAnObject is field test #14's second finding.
//
// time.Time is a struct whose three fields are unexported, so the field walk
// found nothing and published {"type":"object"} — for a value that is
// "0001-01-01T00:00:00Z" on the wire. Timestamps are in nearly every DTO, and
// the silence was the worse half.
func TestTimeIsAStringNotAnObject(t *testing.T) {
	t.Parallel()

	doc, got := emit(t, func(r *transport.Registrar) {
		r.Post("/t", handler[timings, timings]{})
	})
	t.Log(got)

	if !strings.Contains(got, `"format": "date-time"`) {
		t.Errorf("time.Time did not become a date-time string:\n%s", got)
	}
	if strings.Contains(got, `"time.Time"`) {
		t.Errorf("time.Time was registered as a component schema:\n%s", got)
	}
	// A TextMarshaler is a string by encoding/json's own specification — so
	// it is described INLINE, and must not become a component of its own.
	// Asserting on the absence of the $ref is the assertion that matters:
	// `"type": "string"` appears all over a document and would pass on any
	// other field.
	if strings.Contains(got, "textual") {
		t.Errorf("a TextMarshaler was reflected into a component instead of being a string:\n%s", got)
	}
	// A Duration has no marshaller and is nanoseconds on the wire.
	if !strings.Contains(got, `"every"`) || !strings.Contains(got, `"type": "integer"`) {
		t.Errorf("time.Duration did not stay an integer:\n%s", got)
	}
	for _, ref := range doc.Refusals() {
		if strings.Contains(ref.Type, "time.Time") || strings.Contains(ref.Type, "textual") {
			t.Errorf("a describable type was refused: %+v", ref)
		}
	}
}

// TestAJSONMarshalerIsRefusedRatherThanGuessed covers the third rung: a type
// whose wire form is bytes returned by a method reflection cannot read.
//
// warren.md §4.3 has always listed this as refused. Nothing implemented it.
func TestAJSONMarshalerIsRefusedRatherThanGuessed(t *testing.T) {
	t.Parallel()

	doc, got := emit(t, func(r *transport.Registrar) {
		r.Post("/o", handler[opaqueHolder, opaqueHolder]{})
	})
	t.Log(got)

	var found bool
	for _, ref := range doc.Refusals() {
		if strings.Contains(ref.Type, "opaque") {
			found = true
		}
	}
	if !found {
		t.Errorf("a json.Marshaler was described without a refusal; refusals=%+v", doc.Refusals())
	}
	if !strings.Contains(got, "x-warren-undescribed") {
		t.Errorf("the refusal did not reach the operation:\n%s", got)
	}
	// The whole point of Strict(): a document with an undescribed type must
	// not boot silently.
	if _, err := openapi.Emit(tableOf(t, func(r *transport.Registrar) {
		r.Post("/o", handler[opaqueHolder, opaqueHolder]{})
	}), openapi.Strict()); err == nil {
		t.Error("openapi.Strict() booted clean over an undescribed type")
	}
}

// --- refusal coverage for the validate vocabulary ------------------------

type isbnRequest struct {
	ISBN     string `json:"isbn"      validate:"required,isbn13"`
	Borrower string `json:"borrower"  validate:"required,min=2"`
}

// TestAnUnmappedConstraintIsRefusedNotIgnored is finding 8.
//
// `isbn13` IS enforced — a bad ISBN is a 400 naming the field — and was
// published as a bare {"type":"string"}. Ignoring an unmappable tag is right;
// ignoring it silently is what doc.go's contract forbids. The mapping table is
// deliberately not extended: go-playground's isbn13 accepts hyphens and
// spaces, so an invented pattern would be a constraint the client enforces and
// the server does not.
func TestAnUnmappedConstraintIsRefusedNotIgnored(t *testing.T) {
	t.Parallel()

	doc, got := emit(t, func(r *transport.Registrar) {
		r.Post("/isbn", handler[isbnRequest, isbnRequest]{})
	})
	t.Log(got)

	var refused bool
	for _, ref := range doc.Refusals() {
		if strings.Contains(ref.Reason, "isbn13") {
			refused = true
			if ref.Type != "isbn" {
				t.Errorf("the refusal does not name the field: %+v", ref)
			}
		}
		// The control: a token the table DOES map must not be refused, or
		// every document drowns in noise and Strict() becomes unusable.
		for _, mapped := range []string{"`required`", "`min`"} {
			if strings.Contains(ref.Reason, mapped) {
				t.Errorf("a mapped constraint was refused: %+v", ref)
			}
		}
	}
	if !refused {
		t.Errorf("isbn13 was ignored in silence; refusals=%+v", doc.Refusals())
	}
	// It must still be published as far as it IS known.
	if !strings.Contains(got, `"minLength": 2`) {
		t.Errorf("the mapped constraint beside it was lost:\n%s", got)
	}
}

func tableOf(t *testing.T, fn func(r *transport.Registrar)) *transport.Table {
	t.Helper()

	b := transport.NewBuilder(transport.WithValidator(enforcing{}))
	fn(b.For("fixture"))
	if err := b.Failures(); err != nil {
		t.Fatalf("registration: %v", err)
	}
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	return tbl
}

// listBooks is field test #15's finding 7, verbatim: the idiomatic spelling of
// a constrained OPTIONAL query parameter.
type listBooks struct {
	Limit int `query:"limit" validate:"omitempty,min=1,max=100"`
}

type booksPage struct {
	Count int `json:"count"`
}

// TestOmitemptyOnAnOptionalQueryParameterIsNotARefusal — the emitter described
// this route perfectly and then refused it.
//
//	{"name":"limit","in":"query","schema":{"type":"integer","minimum":1,"maximum":100}}
//
// `omitempty` IS expressible: "optional" is `required: false`, which is what
// the parameter already carried. The refusal made openapi.Strict() unusable on
// any service with a constrained optional query parameter — you had to choose
// between the constraint and the strict check.
func TestOmitemptyOnAnOptionalQueryParameterIsNotARefusal(t *testing.T) {
	t.Parallel()

	doc, raw := emit(t, func(r *transport.Registrar) {
		r.Get("/books", app.HandlerFunc[listBooks, booksPage](
			func(context.Context, listBooks) (booksPage, error) { return booksPage{}, nil }))
	})

	for _, ref := range doc.Refusals() {
		if strings.Contains(ref.Reason, "omitempty") {
			t.Errorf("omitempty was refused, so Strict() fails the boot over a route "+
				"this document describes correctly: %+v", ref)
		}
	}

	// It is described, and described as OPTIONAL — which is what omitempty
	// means and the only thing it adds.
	for _, want := range []string{`"minimum": 1`, `"maximum": 100`, `"in": "query"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the parameter lost %s from its schema:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, `"required": true`) {
		t.Errorf("an omitempty parameter was published as required:\n%s", raw)
	}
}

// tenantScoped is field test #16's shape: a field a middleware fills, declared
// unbindable so the bodyless-route check accepts it.
type tenantScoped struct {
	Tenant string `json:"-"`
	Region string `query:"-"`
	Shard  string `param:"-"`
	Status string `query:"status"`
}

type scopedPage struct {
	Count int `json:"count"`
}

// TestAnOptedOutFieldIsNotAPublishedParameter — the second half of the opt-out,
// and the half that is easy to leave undone.
//
// transport stops binding a `json:"-"` / `query:"-"` / `param:"-"` field. If
// openapi kept deriving a parameter from it, the document would publish one
// named "-" — a parameter no request can carry and no client can send, which
// is the invalid-document problem the wildcard checks exist to prevent,
// arriving through the opt-out that fixes a different one.
func TestAnOptedOutFieldIsNotAPublishedParameter(t *testing.T) {
	t.Parallel()

	_, raw := emit(t, func(r *transport.Registrar) {
		r.Get("/skus", app.HandlerFunc[tenantScoped, scopedPage](
			func(context.Context, tenantScoped) (scopedPage, error) { return scopedPage{}, nil }))
	})

	if strings.Contains(raw, `"name": "-"`) {
		t.Errorf("the document publishes a parameter named \"-\":\n%s", raw)
	}
	for _, gone := range []string{"Tenant", "Region", "Shard"} {
		if strings.Contains(raw, gone) {
			t.Errorf("the opted-out field %s reached the document:\n%s", gone, raw)
		}
	}
	// The tagged field beside it must still be described, or the exclusion is
	// too wide.
	if !strings.Contains(raw, `"name": "status"`) {
		t.Errorf("the query parameter beside it was dropped too:\n%s", raw)
	}
}
