package playground_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
	"github.com/MerseniBilel/warren/validate/playground"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// registerUser carries the tag core refuses. This whole module exists so this
// struct can boot.
type registerUser struct {
	Email string `json:"email" validate:"required,email"`
	Name  string `json:"name"  validate:"required,min=2,max=40"`
	Age   int    `json:"age"   validate:"omitempty,gte=18"`
}

// --- the reason this module exists ----------------------------------------

// Core refuses `email` at boot, naming it and telling the user to install
// this module. That instruction has to work.
func TestTheTagCoreRefusesNowBoots(t *testing.T) {
	t.Parallel()

	if _, err := validate.PlanFor[registerUser](validate.Required()); err == nil {
		t.Fatal("core accepted `email` — this module's whole premise is that it does not")
	}
	if _, err := validate.PlanFor[registerUser](playground.New()); err != nil {
		t.Fatalf("the module core tells users to install refused the same type: %v", err)
	}
}

// --- the failure shape ----------------------------------------------------

// A client must not be able to tell which validator is installed: same code,
// same details-keyed-by-field shape, same json names.
func TestFailuresLookLikeCoresToAClient(t *testing.T) {
	t.Parallel()

	rule, err := validate.PlanFor[registerUser](playground.New())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	verr := rule(&registerUser{Email: "not-an-address", Name: "x"})
	if verr == nil {
		t.Fatal("an invalid request passed")
	}
	if !werrors.Is(verr, werrors.CodeInvalid) {
		t.Errorf("code = %v, want INVALID — the same code core returns", verr)
	}

	var werr *werrors.Error
	if !stderrors.As(verr, &werr) {
		t.Fatalf("not a warren error: %T", verr)
	}
	details := werr.Details()
	// Keyed by the JSON name the client sent, never the Go field name: an API
	// reporting "Email" for a field sent as "email" is describing the
	// server's internals.
	if _, ok := details["email"]; !ok {
		t.Errorf("details = %v, want a key for the json name \"email\"", details)
	}
	if _, ok := details["Email"]; ok {
		t.Errorf("details carries the Go field name: %v", details)
	}
	if got := details["email"]; got != "must be a valid email address" {
		t.Errorf("email detail = %v", got)
	}
	if got := details["name"]; got != "must be at least 2 characters" {
		t.Errorf("name detail = %v — min on a string means length", got)
	}
}

// omitempty must still mean optional: an unset field is not a failure.
func TestOmitemptyLeavesAnUnsetFieldAlone(t *testing.T) {
	t.Parallel()

	rule, err := validate.PlanFor[registerUser](playground.New())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if err := rule(&registerUser{Email: "bob@example.com", Name: "Bob"}); err != nil {
		t.Errorf("an unset optional field failed: %v", err)
	}
}

func TestValidRequestPasses(t *testing.T) {
	t.Parallel()

	rule, _ := validate.PlanFor[registerUser](playground.New())
	if err := rule(&registerUser{Email: "bob@example.com", Name: "Bob", Age: 30}); err != nil {
		t.Errorf("a valid request failed: %v", err)
	}
}

// --- the boot-time check --------------------------------------------------

type typo struct {
	Email string `json:"email" validate:"required,emai"`
}

// go-playground PANICS on an unknown tag, and it panics when it VALIDATES —
// so a typo in a field nobody has exercised takes the process down on the
// first request that touches it. Catching it at boot is the whole point.
func TestATypoIsABootFailureNotAProductionPanic(t *testing.T) {
	t.Parallel()

	_, err := validate.PlanFor[typo](playground.New())
	if err == nil {
		t.Fatal("a typo'd constraint planned cleanly — it would have panicked on the first request")
	}
	for _, want := range []string{"emai", "typo", "playground.Register"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic must contain %q:\n%s", want, err)
		}
	}
}

type nested struct {
	Inner struct {
		Code string `json:"code" validate:"nonesuch"`
	} `json:"inner"`
}

func TestTheCheckWalksNestedStructs(t *testing.T) {
	t.Parallel()

	if _, err := validate.PlanFor[nested](playground.New()); err == nil {
		t.Fatal("a bad constraint on a nested field was not caught at boot")
	}
}

type recursive struct {
	Name  string     `json:"name" validate:"required"`
	Child *recursive `json:"child"`
}

// A recursive type must not hang the boot-time walk.
func TestARecursiveTypePlans(t *testing.T) {
	t.Parallel()

	if _, err := validate.PlanFor[recursive](playground.New()); err != nil {
		t.Errorf("a recursive type failed to plan: %v", err)
	}
}

func TestNonStructIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := validate.PlanFor[string](playground.New()); err == nil {
		t.Fatal("a non-struct request type was accepted")
	}
}

// --- custom constraints ---------------------------------------------------

type payment struct {
	Currency string `json:"currency" validate:"required,currency"`
}

func TestRegisterAddsAConstraint(t *testing.T) {
	t.Parallel()

	v := playground.New(playground.Register("currency", func(value any) bool {
		s, ok := value.(string)
		return ok && len(s) == 3
	}))
	rule, err := validate.PlanFor[payment](v)
	if err != nil {
		t.Fatalf("a registered constraint did not plan: %v", err)
	}
	if err := rule(&payment{Currency: "GBP"}); err != nil {
		t.Errorf("a valid value failed: %v", err)
	}
	if err := rule(&payment{Currency: "POUNDS"}); err == nil {
		t.Error("an invalid value passed the registered constraint")
	}
}

// --- end to end through the framework -------------------------------------

type controller struct{}

func (c *controller) register(_ context.Context, cmd registerUser) (struct{}, error) {
	return struct{}{}, nil
}

func (c *controller) Register(r transport.Registrar) {
	transport.Post(r, "/users", app.HandlerFunc[registerUser, struct{}](c.register))
}

// The route closure is compiled at boot step 5 with this validator, so the
// rule runs before the handler — the same path core takes.
func TestItPlugsIntoBootStep5(t *testing.T) {
	t.Parallel()

	var tbl *transport.Table
	m := warren.NewModule("users",
		warren.Controllers(func() *controller { return &controller{} }),
		warren.Providers(func(t *transport.Table) *probe {
			t.Claim(transport.ProtocolHTTP, "test")
			tbl = t
			return &probe{}
		}),
		warren.Eager[*probe](),
	)
	a := warren.New(m)
	if err := a.Validator(playground.New()); err != nil {
		t.Fatalf("Validator: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	_, err := invoke(context.Background(), []byte(`{"email":"nope","name":"Bob"}`))
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Errorf("err = %v, want INVALID — the rule must run before the handler", err)
	}
}

type probe struct{}

// --- diagnostics ----------------------------------------------------------

func TestDiagnosticsAreGolden(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	_, err := validate.PlanFor[typo](playground.New())
	b.WriteString("── unknown constraint\n")
	b.WriteString(err.Error())
	b.WriteString("\n\n")

	_, err = validate.PlanFor[string](playground.New())
	b.WriteString("── not a struct\n")
	b.WriteString(err.Error())
	b.WriteString("\n")

	assertGolden(t, "diagnostics", b.String())
}
