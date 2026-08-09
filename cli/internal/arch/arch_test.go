package arch_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/arch"
	"github.com/MerseniBilel/warren/cli/internal/command"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

var update = flag.Bool("update", false, "rewrite golden files")

// assertGolden pins a diagnostic to a file: the diagnostics ARE the product,
// and untested error text rots immediately.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
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
		t.Errorf("report does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// fixture writes a throwaway Go module and returns its directory.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/fix\n\ngo 1.26.3\n"
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCleanProjectPasses(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go": "package domain\n\ntype User struct{}\n",
		"internal/modules/user/application/register.go": `package application

import "example.com/fix/internal/modules/user/domain"

func New() *domain.User { return nil }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("clean project reported %d violations:\n%s", len(report.Violations), report)
	}
	if report.Packages == 0 {
		t.Error("no packages were analysed — the check would pass vacuously")
	}
}

func TestDomainImportingInfrastructureIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/domain/user.go": `package domain

import "example.com/fix/internal/modules/user/infrastructure"

type User struct{ R infrastructure.Repo }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	v := report.Violations[0]
	if v.Layer != "domain" || v.ImportedLayer != "infrastructure" {
		t.Errorf("violation = %+v", v)
	}
	// The diagnostic must name the file, both layers, and the fix.
	out := report.String()
	for _, want := range []string{"user.go", "domain", "infrastructure", "Declare a port"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic is missing %q:\n%s", want, out)
		}
	}
}

func TestApplicationImportingInfrastructureIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/application/svc.go": `package application

import "example.com/fix/internal/modules/user/infrastructure"

type S struct{ R infrastructure.Repo }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 1 {
		t.Errorf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
}

func TestModuleGoSeesAllFourLayers(t *testing.T) {
	t.Parallel()

	// module.go is the one file permitted to wire the layers together.
	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go":         "package domain\n\ntype User struct{}\n",
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/module.go": `package user

import (
	"example.com/fix/internal/modules/user/domain"
	"example.com/fix/internal/modules/user/infrastructure"
)

var _ = domain.User{}
var _ = infrastructure.Repo{}
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 0 {
		t.Errorf("module.go was flagged:\n%s", report)
	}
}

func TestCrossModuleImportIsAViolation(t *testing.T) {
	t.Parallel()

	// Claim 2 stated mechanically: one feature reaching into another's
	// internals is what makes extraction a rewrite instead of a rewiring.
	dir := fixture(t, map[string]string{
		"internal/modules/billing/domain/invoice.go": "package domain\n\ntype Invoice struct{}\n",
		"internal/modules/user/application/svc.go": `package application

import "example.com/fix/internal/modules/billing/domain"

type S struct{ I domain.Invoice }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	if !strings.Contains(report.String(), "another feature module") {
		t.Errorf("diagnostic does not explain the cross-module rule:\n%s", report)
	}
}

func TestUnlayeredProjectIsExemptNotRefused(t *testing.T) {
	t.Parallel()

	// A plain Go project must run clean rather than being refused: a linter
	// that only works on projects it generated is a linter nobody adopts.
	dir := fixture(t, map[string]string{
		"pkg/a/a.go": "package a\n\ntype A struct{}\n",
		"pkg/b/b.go": "package b\n\nimport \"example.com/fix/pkg/a\"\n\ntype B struct{ A a.A }\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check on a non-Warren project: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("a project with no layers reported violations:\n%s", report)
	}
}

func TestUncompilableProjectStillChecks(t *testing.T) {
	t.Parallel()

	// The fix for a layer violation usually breaks the build first, which is
	// exactly when the check is most needed. Imports are syntactic.
	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/domain/user.go": `package domain

import "example.com/fix/internal/modules/user/infrastructure"

type User struct{ R infrastructure.Repo }

func broken() { this is not go }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) == 0 {
		t.Error("a project that does not compile reported no violations — this is when the check matters most")
	}
}

// TestWarrenItselfIsClean is the dogfood test: the same binary, the same
// analyzer, run over the framework's own repository. "The same warren lint
// arch that ships to users" is a claim about the code path, and this is
// what keeps it honest.
func TestWarrenItselfIsClean(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "warren.md")); statErr != nil {
		t.Skip("not running inside the Warren repository")
	}
	report, err := arch.Check(root, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("Warren violates its own rules:\n%s", report)
	}
	if report.Packages < 10 {
		t.Errorf("only %d packages analysed — the dogfood test would pass vacuously", report.Packages)
	}
}

// TestGeneratedAppIsClean closes the loop: what the CLI scaffolds must pass
// the linter the same CLI ships.
func TestGeneratedAppIsClean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the scaffold the CLI generates breaks the rules the CLI enforces:\n%s", report)
	}
}

// TestRelativeRootIsNotSkipped pins a vacuous pass this linter shipped with
// for one commit: a root named ".." or "." starts with a dot, and the
// hidden-directory skip swallowed the whole tree — reporting "no violations
// in 0 packages", which reads like success.
func TestRelativeRootIsNotSkipped(t *testing.T) {
	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go": "package domain\n\ntype User struct{}\n",
	})
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	report, err := arch.Check("..", arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Packages == 0 {
		t.Error(`Check("..") analysed 0 packages — a vacuous pass that reads like success`)
	}
}

// TestHandlerImportingTransportIsAViolation — invariant 5, "handlers import
// no transport package", is the rule Warren repeats most often and the one
// `lint arch` did not check. A field test put both `warren/transport` and
// `net/http` into an application-layer file and got:
//
//	$ warren lint arch
//	No violations in 17 packages.   EXIT=0
//
// The layer and cross-module rules only ever examined imports beginning
// with the project's OWN module path; everything else was waved through as
// "third party and stdlib are not this rule's business". For this rule they
// are exactly its business.
func TestHandlerImportingTransportIsAViolation(t *testing.T) {
	t.Parallel()

	for _, imported := range []string{
		"github.com/MerseniBilel/warren/transport",
		"github.com/MerseniBilel/warren/transport/http",
		"net/http",
		"github.com/go-chi/chi/v5",
		"google.golang.org/grpc",
	} {
		t.Run(imported, func(t *testing.T) {
			t.Parallel()
			dir := fixture(t, map[string]string{
				"internal/modules/user/application/register.go": "package application\n\nimport _ \"" + imported + "\"\n",
			})
			report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(report.Violations) != 1 {
				t.Fatalf("importing %q from the application layer reported %d violations, want 1:\n%s",
					imported, len(report.Violations), report)
			}
			if got := report.Violations[0].Rule; got != "transport" {
				t.Errorf("rule = %q, want %q", got, "transport")
			}
			for _, want := range []string{"transport", imported, "register.go"} {
				if !strings.Contains(report.String(), want) {
					t.Errorf("the report does not mention %q:\n%s", want, report)
				}
			}
		})
	}
}

// TestTheControllerMayImportTransport — the controller is where a use case
// meets a protocol, and it lives at the module root, unlayered. A rule that
// refused it would refuse the framework's own generated code.
func TestTheControllerMayImportTransport(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/controller.go": "package user\n\nimport _ \"github.com/MerseniBilel/warren/transport\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the controller was refused for importing transport:\n%s", report)
	}
}

// TestInfrastructureMayUseNetHTTP — an adapter calling a third-party API
// over HTTP is the ordinary reason infrastructure exists. Refusing it would
// make the rule wrong more often than right.
func TestInfrastructureMayUseNetHTTP(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/pricing.go": "package infrastructure\n\nimport _ \"net/http\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("an infrastructure adapter was refused for using net/http:\n%s", report)
	}
}

// TestDomainImportingTransportIsNotAHandlerProblem — a domain type holding an
// *http.Client is a transport violation, but it is the DOMAIN's, and the
// report described the wrong one:
//
//	✗ handler imports a transport package
//	  ...
//	  Fix one of:
//	    • Move the routing to the feature's controller.go, ...
//
// There is no routing to move. A domain type is not a handler, it has no
// Register call, and the first fix offered is meaningless in the file it is
// printed about — which is how a reader concludes the linter does not
// understand their code and switches it off.
func TestDomainImportingTransportIsNotAHandlerProblem(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go": "package domain\n\nimport \"net/http\"\n\ntype User struct{ C *http.Client }\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("got %d violations, want 1:\n%s", len(report.Violations), report)
	}
	if got := report.Violations[0].Layer; got != "domain" {
		t.Errorf("layer = %q, want %q", got, "domain")
	}

	out := report.String()
	if strings.Contains(out, "handler imports a transport package") {
		t.Errorf("a domain violation is reported as a handler's:\n%s", out)
	}
	if strings.Contains(out, "Move the routing") {
		t.Errorf("the report offers a fix that does not apply to a domain type:\n%s", out)
	}
	for _, want := range []string{"domain", "net/http", "user.go", "infrastructure"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
}

// TestApplicationImportingTransportStillReadsAsAHandler — the corrected
// wording must not cost the case it was written for. A use case handler DOES
// live in the application layer, and moving the routing to controller.go is
// exactly right there.
func TestApplicationImportingTransportStillReadsAsAHandler(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/application/register.go": "package application\n\nimport _ \"net/http\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	out := report.String()
	if !strings.Contains(out, "handler imports a transport package") {
		t.Errorf("an application-layer violation lost its handler wording:\n%s", out)
	}
	if !strings.Contains(out, "Move the routing") {
		t.Errorf("an application-layer violation lost the fix that applies to it:\n%s", out)
	}
}

// TestHandlerReachingTransportThroughAHelperIsAViolation — the rule was
// enforced against DIRECT imports only, and one level of indirection defeated
// it entirely. Worse, the indirection is the one GETTING_STARTED walks you
// into: it tells you to write your own edge middleware with whttp.WriteError,
// and the obvious factoring puts that beside the tenant reader and the
// policies in one internal/auth package — which the application layer then
// imports for the tenant.
//
// Field test #7, on a project the linter called clean:
//
//	$ warren lint arch
//	No violations in 19 packages.
//	$ go list -deps .../ticket/application | grep -E 'net/http|transport'
//	github.com/MerseniBilel/warren/transport
//	net/http
//	github.com/MerseniBilel/warren/transport/http
//
// README calls "a handler imports no transport package" the entire point.
func TestHandlerReachingTransportThroughAHelperIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/auth/auth.go": `package auth

import "net/http"

func TenantOf(r *http.Request) string { return "" }
`,
		"internal/modules/ticket/application/open.go": `package application

import "example.com/fix/internal/auth"

func Open() string { return auth.TenantOf(nil) }
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report.String())
	}
	out := report.String()
	// The chain is the whole value: the offending import is in a package the
	// reader did not suspect, and naming only the endpoints sends them
	// looking in the wrong file.
	for _, want := range []string{
		"internal/modules/ticket/application",
		"internal/auth",
		"net/http",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
}

// TestInfrastructureMayReachTransportThroughAHelper — an adapter calling a
// third-party API over net/http is the ordinary reason it exists, and that
// exemption must survive indirection too or the linter becomes noise.
func TestInfrastructureMayReachTransportThroughAHelper(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/httpclient/client.go": `package httpclient

import "net/http"

func Get(url string) (*http.Response, error) { return nil, nil }
`,
		"internal/modules/ticket/infrastructure/pager.go": `package infrastructure

import "example.com/fix/internal/httpclient"

func Page() { _, _ = httpclient.Get("") }
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("infrastructure was refused a transport dependency:\n%s", report.String())
	}
}

// TestATransportFreeHelperIsFine — the fix the field test applied by hand
// (split internal/auth into a transport-free half and an authhttp half) must
// come back clean, or the linter is telling people to do something that does
// not satisfy it.
func TestATransportFreeHelperIsFine(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/auth/auth.go": `package auth

type Identity struct{ Tenant string }

func TenantOf(id Identity) string { return id.Tenant }
`,
		"internal/authhttp/edge.go": `package authhttp

import (
	"net/http"

	"example.com/fix/internal/auth"
)

func Middleware(next http.Handler) http.Handler { _ = auth.Identity{}; return next }
`,
		"internal/modules/ticket/application/open.go": `package application

import "example.com/fix/internal/auth"

func Open() string { return auth.TenantOf(auth.Identity{}) }
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the documented fix does not satisfy the linter:\n%s", report.String())
	}
}

// TestLayerViolationThroughAHelperIsAViolation — field test #8, defect 2.
// The transitive check shipped for the TRANSPORT rule only, so the layer
// rule stayed depth-1 and a package outside internal/modules/ laundered it:
//
//	$ warren lint arch
//	No violations in 25 packages.
//	LINT_EXIT=0
//	$ go list -deps .../stock/application | grep -c stock/infrastructure
//	1                      # it really does depend on it
//
// findTransportChains' own doc states the reasoning that applies verbatim
// here: "the direct rule reads one file and is easy to satisfy by accident:
// move the import into a helper and the check goes quiet."
func TestLayerViolationThroughAHelperIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/bridge/bridge.go": `package bridge

import "example.com/fix/internal/modules/stock/infrastructure"

func Repo() any { return infrastructure.New() }
`,
		"internal/modules/stock/infrastructure/repo.go": "package infrastructure\n\nfunc New() any { return nil }\n",
		"internal/modules/stock/application/receive.go": `package application

import "example.com/fix/internal/bridge"

var _ = bridge.Repo
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report.String())
	}
	out := report.String()
	for _, want := range []string{"internal/bridge", "stock/infrastructure", "stock/application"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
}

// TestCrossModuleThroughAHelperIsAViolation — the same laundering, on the
// rule that makes extracting a module a rewiring rather than a rewrite. The
// field test used a TYPE ALIAS in the shared package, so the two modules
// share literally the same Go type and the coupling is total.
func TestCrossModuleThroughAHelperIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/stock/domain/item.go": "package domain\n\ntype Item struct{}\n",
		"internal/shared/types.go": `package shared

import "example.com/fix/internal/modules/stock/domain"

type Item = domain.Item
`,
		"internal/modules/replenishment/application/demand.go": `package application

import "example.com/fix/internal/shared"

var _ shared.Item
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report.String())
	}
	if out := report.String(); !strings.Contains(out, "internal/shared") || !strings.Contains(out, "stock/domain") {
		t.Errorf("the report does not name the chain:\n%s", out)
	}
}

// TestAHelperUsedByOneModuleOnlyIsFine — a shared package that reaches into
// NO other feature is ordinary code, and reporting it would make the rule
// noise. This is the control: without it the check could pass by reporting
// every helper.
func TestAHelperUsedByOneModuleOnlyIsFine(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/idgen/idgen.go": "package idgen\n\nfunc New() string { return \"\" }\n",
		"internal/modules/stock/application/receive.go": `package application

import "example.com/fix/internal/idgen"

var _ = idgen.New
`,
		"internal/modules/stock/domain/item.go": "package domain\n\ntype Item struct{}\n",
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("an innocent helper was reported:\n%s", report.String())
	}
}

// TestALayeredIntermediateIsNotReportedTwice — a chain through a package
// that is ITSELF layered is that package's own violation, reported at its
// own import. Reporting it again at every package downstream turns one
// mistake into a wall of findings.
func TestALayeredIntermediateIsNotReportedTwice(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/stock/infrastructure/repo.go": "package infrastructure\n\nfunc New() any { return nil }\n",
		"internal/modules/stock/domain/item.go": `package domain

import "example.com/fix/internal/modules/stock/infrastructure"

var _ = infrastructure.New
`,
		"internal/modules/stock/application/receive.go": `package application

import "example.com/fix/internal/modules/stock/domain"

var _ = domain.New
`,
	})

	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Exactly one: domain -> infrastructure. Not a second one at application.
	if len(report.Violations) != 1 {
		t.Errorf("violations = %d, want exactly 1 (domain's own):\n%s", len(report.Violations), report.String())
	}
}

// TestTheSanctionedCrossFeaturePortLintsClean is the test that proves ruling
// 4 of 2026-08-08 is actually reachable.
//
// Before it, the two shipped tools instructed users in a circle. Boot said
// "Add to ordering's module: warren.Exports[domain.OrderRepository]()", and
// `warren lint arch` then refused the import that naming the type requires —
// so warren.Exports between two feature modules was structurally impossible.
//
// The sanctioned path: the port moves to a self-contained package OUTSIDE
// internal/modules/, which featureOf already exempts. No rule changed; the
// remedy text did.
func TestTheSanctionedCrossFeaturePortLintsClean(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		// The contract: an interface and its own types, importing no feature.
		"internal/contracts/ordering/orders.go": `package ordering

import "context"

type Order struct{ ID string }

type Orders interface {
	Find(ctx context.Context, id string) (Order, error)
}
`,
		// The owner implements it.
		"internal/modules/ordering/infrastructure/repo.go": `package infrastructure

import (
	"context"

	"example.com/fix/internal/contracts/ordering"
)

type Repo struct{}

func (Repo) Find(context.Context, string) (ordering.Order, error) { return ordering.Order{}, nil }
`,
		// The other feature asks through the contract, never through ordering.
		"internal/modules/fulfillment/application/ship.go": `package application

import (
	"context"

	"example.com/fix/internal/contracts/ordering"
)

type Ship struct{ Orders ordering.Orders }

func (s Ship) Do(ctx context.Context, id string) error {
	_, err := s.Orders.Find(ctx, id)
	return err
}
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the sanctioned cross-feature port was refused — the ruling is unreachable:\n%s", report)
	}
}

// A contract package that absorbs a feature's types is the drift this shape
// invites, and findLaunderedImports already catches it: the contract is a
// helper, the chain is traversed, and the asking feature is reported.
func TestAContractPackageThatImportsAFeatureIsReported(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/ordering/domain/order.go": "package domain\n\ntype Order struct{}\n",
		"internal/contracts/ordering/orders.go": `package ordering

import "example.com/fix/internal/modules/ordering/domain"

type Orders interface{ Find() domain.Order }
`,
		"internal/modules/fulfillment/application/ship.go": `package application

import "example.com/fix/internal/contracts/ordering"

type Ship struct{ Orders ordering.Orders }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) == 0 {
		t.Fatalf("a contract package importing a feature was not reported:\n%s", report)
	}
	if !strings.Contains(report.String(), "fulfillment") {
		t.Errorf("the report does not name the feature that reached through it:\n%s", report)
	}
	if report.Violations[0].Rule != "cross-module-chain" {
		t.Errorf("reported as %q, want cross-module-chain — the laundering rule, not an incidental match:\n%s",
			report.Violations[0].Rule, report)
	}
}

// The remedy must name the sanctioned path, or the user is told what is wrong
// and not what to do — which is how the contradiction shipped.
func TestTheCrossModuleRemedyNamesTheContractPackage(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/billing/domain/invoice.go": "package domain\n\ntype Invoice struct{}\n",
		"internal/modules/user/application/svc.go": `package application

import "example.com/fix/internal/modules/billing/domain"

type S struct{ I domain.Invoice }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	got := report.String()
	for _, want := range []string{"internal/contracts/", "warren g consumer", "warren.Exports"} {
		if !strings.Contains(got, want) {
			t.Errorf("the remedy does not mention %q:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// F3 — the report says what it did NOT check.
// ---------------------------------------------------------------------------

// twoFeatures returns the same project twice over: once under the layout
// GETTING_STARTED §1-§5 teaches (internal/<feature>/…), once under the layout
// `warren new` generates (internal/modules/<feature>/…). The import from one
// feature into another's domain is byte-identical in both.
func twoFeatures(t *testing.T, root string) string {
	t.Helper()
	return fixture(t, map[string]string{
		root + "/tasks/domain/task.go": "package domain\n\ntype Task struct{}\n",
		root + "/notes/domain/note.go": "package domain\n\ntype Note struct{}\n",
		root + "/notes/application/attach.go": `package application

import "example.com/fix/` + root + `/tasks/domain"

type Attach struct{ T domain.Task }
`,
		root + "/notes/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
	})
}

// TestZeroFeaturesIsDisclosed — field test #13, finding F3. Under the layout
// GETTING_STARTED §1-§5 teaches, featureOf returns "" for every package, the
// cross-module rule and its through-a-helper variant are both skipped, and a
// real cross-feature import printed:
//
//	No violations in 7 packages.   EXIT=0
//
// Nothing said half the rule set had not run. A check that did not run must
// never read as a check that passed.
func TestZeroFeaturesIsDisclosed(t *testing.T) {
	t.Parallel()

	report, err := arch.Check(twoFeatures(t, "internal"), arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Features != 0 {
		t.Errorf("Features = %d, want 0 — no `modules` segment in this tree", report.Features)
	}
	out := report.String()
	for _, want := range []string{
		"NOT checked: the cross-module rule",
		"modules",
		"internal/modules/<feature>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not disclose %q:\n%s", want, out)
		}
	}
	assertGolden(t, "report_clean_no_features", out)
}

// TestFeatureCountIsReportedWhenPresent — the same tree, one segment moved.
// The rule runs, the violation is found, and the report says how much it
// compared rather than repeating the disclosure.
func TestFeatureCountIsReportedWhenPresent(t *testing.T) {
	t.Parallel()

	report, err := arch.Check(twoFeatures(t, "internal/modules"), arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Features != 2 {
		t.Errorf("Features = %d, want 2", report.Features)
	}
	out := report.String()
	if !strings.Contains(out, "Checked 2 feature modules for cross-module imports.") {
		t.Errorf("the report does not say what it compared:\n%s", out)
	}
	if strings.Contains(out, "NOT checked") {
		t.Errorf("a rule that RAN was disclosed as skipped:\n%s", out)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1 — the cross-feature import:\n%s", len(report.Violations), out)
	}
	assertGolden(t, "report_features_present", out)
}

// TestDisclosureAppearsWithViolationsToo — the paragraph is about the rule
// set, not about the outcome. A report with findings is still a report that
// silently skipped half its rules.
func TestDisclosureAppearsWithViolationsToo(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/notes/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/notes/domain/note.go": `package domain

import "example.com/fix/internal/notes/infrastructure"

type Note struct{ R infrastructure.Repo }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	out := report.String()
	if !strings.Contains(out, "NOT checked: the cross-module rule") {
		t.Errorf("a report WITH violations dropped the disclosure:\n%s", out)
	}
	assertGolden(t, "report_violations_no_features", out)
}

// TestDisclosureDoesNotChangeExitCode — a disclosure is not a failure. A tool
// that started failing builds over how a directory is named is a tool people
// switch off, which is the outcome every ruling in this round avoids. Run
// through the real command, because the exit code is the command's.
func TestDisclosureDoesNotChangeExitCode(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/notes/domain/note.go": "package domain\n\ntype Note struct{}\n",
	})
	root := command.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"lint", "arch", dir})
	err := root.Execute()
	if code := command.ExitCode(err); code != 0 {
		t.Errorf("exit = %d, want 0 — the disclosure is not a violation:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "NOT checked: the cross-module rule") {
		t.Errorf("the command's own output does not carry the disclosure:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// F4 — a driver in domain/ or application/ is a violation.
// ---------------------------------------------------------------------------

// TestPgxInApplicationIsAViolation is the F4 regression test. README:51-54
// calls "a handler imports no transport package — no net/http, no pgx, no
// kgo" the entire point; net/http was caught and every driver in the sentence
// was waved through. Verified on a plain `warren new` scaffold: `go build`
// passed and `warren lint arch .` printed "No violations in 10 packages."
func TestPgxInApplicationIsAViolation(t *testing.T) {
	t.Parallel()

	for _, imported := range []string{
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgx/v5/pgxpool",
		"database/sql",
		"github.com/lib/pq",
		"github.com/go-sql-driver/mysql",
		"go.mongodb.org/mongo-driver/mongo",
		"github.com/redis/go-redis/v9",
		"github.com/twmb/franz-go/pkg/kgo",
		"github.com/segmentio/kafka-go",
		"github.com/IBM/sarama",
		"github.com/rabbitmq/amqp091-go",
		"github.com/nats-io/nats.go",
	} {
		t.Run(imported, func(t *testing.T) {
			t.Parallel()
			dir := fixture(t, map[string]string{
				"internal/modules/orders/application/place.go": "package application\n\nimport _ \"" + imported + "\"\n",
			})
			report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(report.Violations) != 1 {
				t.Fatalf("importing %q from the application layer reported %d violations, want 1:\n%s",
					imported, len(report.Violations), report)
			}
			if got := report.Violations[0].Rule; got != "driver" {
				t.Errorf("rule = %q, want %q", got, "driver")
			}
			out := report.String()
			for _, want := range []string{"handler imports a driver", imported, "place.go"} {
				if !strings.Contains(out, want) {
					t.Errorf("the report does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestPgxInDomainGetsTheDomainRemedy — the two layers are not the same
// mistake, and the domain's version of the rule is the stronger one. "Move
// the routing to the controller" is nonsense for a pgx import; so is telling
// a value object it has a handler.
func TestPgxInDomainGetsTheDomainRemedy(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/domain/order.go": "package domain\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	out := report.String()
	if !strings.Contains(out, "the domain imports a driver") {
		t.Errorf("a domain violation is reported as a handler's:\n%s", out)
	}
	if strings.Contains(out, "Move the routing") {
		t.Errorf("the report offers a transport fix for a driver import:\n%s", out)
	}
	for _, want := range []string{"depends on", "infrastructure", "order.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
	assertGolden(t, "driver_domain", out)
}

// TestDatabaseSQLDriverInDomainIsAViolation — the legitimate-looking
// minority: a value object implementing driver.Valuer so it can be stored.
// That IS the coupling Warren argues against, and the remedy must name the
// case rather than leave the reader guessing why their Money type is being
// reported.
func TestDatabaseSQLDriverInDomainIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/domain/money.go": `package domain

import "database/sql/driver"

type Money struct{ Cents int64 }

func (m Money) Value() (driver.Value, error) { return m.Cents, nil }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	if !strings.Contains(report.String(), "driver.Valuer") {
		t.Errorf("the remedy does not name the case it is reporting:\n%s", report)
	}
}

// TestWarrenPersistencePostgresInApplicationIsAViolation — Warren's own
// adapter modules are drivers like any other. A use case naming
// warren/persistence/postgres can only run where a pool can be built.
func TestWarrenPersistencePostgresInApplicationIsAViolation(t *testing.T) {
	t.Parallel()

	for _, imported := range []string{
		"github.com/MerseniBilel/warren/persistence/postgres",
		"github.com/MerseniBilel/warren/persistence/mysql",
		"github.com/MerseniBilel/warren/persistence/mongo",
		"github.com/MerseniBilel/warren/persistence/redis",
		"github.com/MerseniBilel/warren/broker/kafka",
		"github.com/MerseniBilel/warren/broker/rabbitmq",
		"github.com/MerseniBilel/warren/broker/nats",
		"github.com/MerseniBilel/warren/broker/memory",
		"github.com/MerseniBilel/warren/observability",
	} {
		t.Run(imported, func(t *testing.T) {
			t.Parallel()
			dir := fixture(t, map[string]string{
				"internal/modules/orders/application/place.go": "package application\n\nimport _ \"" + imported + "\"\n",
			})
			report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
			if len(report.Violations) != 1 {
				t.Fatalf("importing %q reported %d violations, want 1:\n%s", imported, len(report.Violations), report)
			}
			if got := report.Violations[0].Rule; got != "driver" {
				t.Errorf("rule = %q, want driver", got)
			}
		})
	}
}

// TestWarrenPersistenceContractPackageIsNotAViolation is the prefix guard.
// warren/persistence and warren/broker are CONTRACT packages: a domain naming
// persistence.UnitOfWork is the pattern, not the violation. Listing
// warren/persistence/postgres must never catch warren/persistence.
func TestWarrenPersistenceContractPackageIsNotAViolation(t *testing.T) {
	t.Parallel()

	for _, imported := range []string{
		"github.com/MerseniBilel/warren/persistence",
		"github.com/MerseniBilel/warren/broker",
		"github.com/MerseniBilel/warren/errors",
	} {
		for _, layer := range []string{"domain", "application"} {
			t.Run(imported+"/"+layer, func(t *testing.T) {
				t.Parallel()
				dir := fixture(t, map[string]string{
					"internal/modules/orders/" + layer + "/port.go": "package " + layer + "\n\nimport _ \"" + imported + "\"\n",
				})
				report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if len(report.Violations) != 0 {
					t.Errorf("the %s layer was refused %q — that is the pattern, not the violation:\n%s",
						layer, imported, report)
				}
			})
		}
	}
}

// TestDriverInInfrastructureIsClean — an adapter holding a driver is the
// reason infrastructure exists. Refusing it would make the rule wrong more
// often than right.
func TestDriverInInfrastructureIsClean(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/infrastructure/repo.go": "package infrastructure\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("an infrastructure adapter was refused its driver:\n%s", report)
	}
}

// TestDriverInControllerIsClean — the controller is unlayered and exempt by
// construction, exactly as it is for transport.
func TestDriverInControllerIsClean(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/controller.go": "package orders\n\nimport _ \"database/sql\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the controller was refused a driver:\n%s", report)
	}
}

// TestDriverThroughAHelperIsAViolation — the direct rule reads one file,
// which makes it satisfiable by accident: move the import into an unlayered
// helper and the check goes quiet while the dependency is exactly as real.
// The transport rule already learned this the expensive way.
func TestDriverThroughAHelperIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/store/store.go": `package store

import "github.com/jackc/pgx/v5"

func Pool() *pgx.Conn { return nil }
`,
		"internal/modules/orders/application/place.go": `package application

import "example.com/fix/internal/store"

var _ = store.Pool
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	v := report.Violations[0]
	if v.Rule != "driver-chain" {
		t.Errorf("rule = %q, want driver-chain", v.Rule)
	}
	// The line reported is the handler's OWN import of the helper: that is
	// the edge the reader owns and the one they will change.
	if v.File != "internal/modules/orders/application/place.go" {
		t.Errorf("file = %q, want the handler's own import line", v.File)
	}
	if got := strings.Join(v.Via, " → "); got != "example.com/fix/internal/modules/orders/application → example.com/fix/internal/store" {
		t.Errorf("chain = %q", got)
	}
	out := report.String()
	for _, want := range []string{"internal/store", "github.com/jackc/pgx/v5", "place.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
	assertGolden(t, "driver_chain", out)
}

// TestDriverChainReportsShortestPath — a reader given a four-hop path when a
// one-hop path exists will not believe the tool.
func TestDriverChainReportsShortestPath(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/long/long.go":   "package long\n\nimport _ \"example.com/fix/internal/deep\"\n",
		"internal/deep/deep.go":   "package deep\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
		"internal/short/short.go": "package short\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
		"internal/modules/orders/application/place.go": `package application

import (
	_ "example.com/fix/internal/long"
	_ "example.com/fix/internal/short"
)
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	v := report.Violations[0]
	if len(v.Via) != 2 || !strings.HasSuffix(v.Via[1], "internal/short") {
		t.Errorf("chain = %v, want the two-hop path through internal/short", v.Via)
	}
}

// TestDriverInApplicationGolden pins the handler remedy, which is the half of
// the rule that must NOT read like the transport one.
func TestDriverInApplicationGolden(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/application/place.go": "package application\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertGolden(t, "driver_application", report.String())
}

// TestDriverInModuleGoIsExempt — module.go is the one file permitted to wire
// a feature together, which is where a driver is handed to the repository
// that will hold it. It is skipped before it is parsed, so its imports reach
// neither the direct rule nor the graph the chain search walks: the exemption
// holds for the driver rule as it does for every other.
func TestDriverInModuleGoIsExempt(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/application/module.go": "package application\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
		"internal/modules/orders/application/place.go":  "package application\n\ntype Place struct{}\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("module.go was refused its driver:\n%s", report)
	}
}

// TestATransportAndADriverAreTwoFindings — the two rules share one
// breadth-first search and one "already reported directly" map, and that map
// is keyed per rule for a reason: a handler that imports both has made two
// different mistakes with two different remedies, and suppressing one behind
// the other is how a fix ships half done.
func TestATransportAndADriverAreTwoFindings(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/orders/application/place.go": `package application

import (
	_ "github.com/jackc/pgx/v5"
	_ "net/http"
)
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	rules := map[string]int{}
	for _, v := range report.Violations {
		rules[v.Rule]++
	}
	if rules["transport"] != 1 || rules["driver"] != 1 || len(report.Violations) != 2 {
		t.Errorf("violations = %v, want one transport and one driver:\n%s", rules, report)
	}
}

// TestInfrastructureMayReachADriverThroughAHelper — an adapter is where a
// driver lives, and that exemption must survive indirection or the linter
// becomes noise. An internal/store package handing a pool to a repository is
// the ordinary shape.
func TestInfrastructureMayReachADriverThroughAHelper(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/store/store.go": "package store\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
		"internal/modules/orders/infrastructure/repo.go": `package infrastructure

import _ "example.com/fix/internal/store"
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("infrastructure was refused a driver dependency:\n%s", report)
	}
}

// TestDomainReachingADriverThroughAHelperGetsTheDomainWording — the domain's
// version of the rule is the stronger one, and it must not be lost to
// indirection. A domain told to split a package it does not own, when what it
// has is a type that knows how it is stored, is a domain whose owner switches
// the linter off.
func TestDomainReachingADriverThroughAHelperGetsTheDomainWording(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/store/store.go": "package store\n\nimport _ \"database/sql\"\n",
		"internal/modules/orders/domain/money.go": `package domain

import _ "example.com/fix/internal/store"
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	out := report.String()
	if !strings.Contains(out, "the domain reaches a driver") {
		t.Errorf("a domain chain is reported as a handler's:\n%s", out)
	}
	if !strings.Contains(out, "driver.Valuer") {
		t.Errorf("the domain remedy was lost to indirection:\n%s", out)
	}
}
