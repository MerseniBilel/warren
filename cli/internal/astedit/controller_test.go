package astedit_test

import (
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/astedit"
)

// The controller is the one file `warren g command` could not touch, so
// every generated use case ended with a twelve-line manual patch telling
// the user to edit a struct, a constructor and a method body by hand. Four
// commands meant four patches, and the file the patch names is one no
// generator creates.
//
// These are the three splices that removes. Each is idempotent, because a
// generator is re-run.

const controller = `package catalog

import (
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/transport"

	"example.com/app/internal/modules/catalog/application"
)

// Controller exposes this feature's use cases.
type Controller struct {
	createProduct app.Handler[application.CreateProduct, application.CreateProductResult]
}

func NewController(
	createProduct app.Handler[application.CreateProduct, application.CreateProductResult],
) *Controller {
	return &Controller{createProduct: createProduct}
}

// Register declares the routes.
func (c *Controller) Register(r transport.Registrar) {
	transport.Post(r, "/products", c.createProduct)
}
`

func TestAddStructField(t *testing.T) {
	t.Parallel()

	got, err := astedit.AddStructField([]byte(controller), "Controller",
		"discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult]")
	if err != nil {
		t.Fatalf("AddStructField: %v", err)
	}
	// Whitespace-insensitive: gofmt ALIGNS struct fields, so the rendered
	// text carries however many spaces the widest neighbour needs.
	if !hasDecl(string(got), "discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult]") {
		t.Errorf("field not added:\n%s", got)
	}
	// The existing field survives, and so does its comment above the struct.
	for _, want := range []string{"createProduct app.Handler[application.CreateProduct", "// Controller exposes this feature's use cases."} {
		if !strings.Contains(string(got), want) {
			t.Errorf("splice lost %q:\n%s", want, got)
		}
	}

	// Idempotent: a generator gets re-run, and a duplicate field does not
	// compile.
	again, err := astedit.AddStructField(got, "Controller", "discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult]")
	if err != nil {
		t.Fatalf("second AddStructField: %v", err)
	}
	if n := countDecl(string(again), "discontinue", "app.Handler"); n != 1 {
		t.Errorf("the field appears %d times after a re-run, want 1:\n%s", n, again)
	}
}

func TestAddConstructorParam(t *testing.T) {
	t.Parallel()

	got, err := astedit.AddConstructorParam([]byte(controller), "NewController", "Controller",
		"discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult]")
	if err != nil {
		t.Fatalf("AddConstructorParam: %v", err)
	}
	// Both halves: the parameter AND the struct literal field. Either alone
	// leaves the file uncompilable.
	if !hasDecl(string(got), "discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult],") {
		t.Errorf("parameter not added:\n%s", got)
	}
	if !strings.Contains(string(got), "discontinue: discontinue") {
		t.Errorf("struct literal field not added:\n%s", got)
	}

	again, err := astedit.AddConstructorParam(got, "NewController", "Controller", "discontinue", "app.Handler[application.Discontinue, application.DiscontinueResult]")
	if err != nil {
		t.Fatalf("second AddConstructorParam: %v", err)
	}
	if n := strings.Count(string(again), "discontinue: discontinue"); n != 1 {
		t.Errorf("the literal field appears %d times after a re-run, want 1:\n%s", n, again)
	}
}

func TestAddStatement(t *testing.T) {
	t.Parallel()

	got, err := astedit.AddStatement([]byte(controller), "Register",
		`transport.Post(r, "/discontinue", c.discontinue)`)
	if err != nil {
		t.Fatalf("AddStatement: %v", err)
	}
	if !strings.Contains(string(got), `transport.Post(r, "/discontinue", c.discontinue)`) {
		t.Errorf("statement not added:\n%s", got)
	}
	// Order matters for readability, not correctness: the new route goes
	// last, after the ones already there.
	first := strings.Index(string(got), `"/products"`)
	second := strings.Index(string(got), `"/discontinue"`)
	if first < 0 || second < first {
		t.Errorf("the new route did not land after the existing one:\n%s", got)
	}

	again, err := astedit.AddStatement(got, "Register", `transport.Post(r, "/discontinue", c.discontinue)`)
	if err != nil {
		t.Fatalf("second AddStatement: %v", err)
	}
	if n := strings.Count(string(again), `"/discontinue"`); n != 1 {
		t.Errorf("the route appears %d times after a re-run, want 1:\n%s", n, again)
	}
}

// TestControllerEditsRefuseAMissingTarget — a generator that silently does
// nothing is how a user ends up with a handler nothing serves and no idea
// why.
func TestControllerEditsRefuseAMissingTarget(t *testing.T) {
	t.Parallel()

	if _, err := astedit.AddStructField([]byte(controller), "Nope", "x", "int"); err == nil {
		t.Error("AddStructField on a type that does not exist reported success")
	}
	if _, err := astedit.AddConstructorParam([]byte(controller), "NewNope", "Nope", "x", "int"); err == nil {
		t.Error("AddConstructorParam on a function that does not exist reported success")
	}
	if _, err := astedit.AddStatement([]byte(controller), "Nope", "x()"); err == nil {
		t.Error("AddStatement on a function that does not exist reported success")
	}
}

// hasDecl reports whether src declares name with the given type, ignoring
// the alignment gofmt inserts between them.
func hasDecl(src, name, typ string) bool { return countDecl(src, name, typ) > 0 }

func countDecl(src, name, typ string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name && strings.HasPrefix(strings.Join(fields[1:], " "), typ) {
			n++
		}
	}
	return n
}
