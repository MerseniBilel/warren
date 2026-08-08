package arch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/arch"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

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
