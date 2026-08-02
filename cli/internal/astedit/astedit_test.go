package astedit_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/astedit"
)

// commented is a module file dense with comments and blank lines — the
// thing a reprinting editor would quietly reformat.
const commented = `// Package user is the user feature.
package user

import (
	"sync"

	"github.com/MerseniBilel/warren"

	"example.com/app/internal/modules/user/application"
	"example.com/app/internal/modules/user/infrastructure"
)

// Module declares the feature.
var Module = sync.OnceValue(func() warren.Module {
	return warren.NewModule("user",
		warren.Providers(
			// The repository first: everything else depends on it.
			infrastructure.NewUserRepository,

			application.NewRegisterUserHandler, // the use case
		),
	)
})
`

func TestAddArgumentInsertsAndPreservesEverythingElse(t *testing.T) {
	t.Parallel()

	out, err := astedit.AddArgument([]byte(commented), "warren.Providers", "application.NewGetUserHandler")
	if err != nil {
		t.Fatalf("AddArgument: %v", err)
	}

	// Exactly one line differs. This is a stronger claim than "comments
	// preserved", and it is the property splicing buys over reprinting.
	added, removed := diffLines(commented, string(out))
	if len(removed) != 0 {
		t.Errorf("the edit removed %d lines:\n%s", len(removed), strings.Join(removed, "\n"))
	}
	if len(added) != 1 {
		t.Fatalf("the edit added %d lines, want exactly 1:\n%s", len(added), strings.Join(added, "\n"))
	}
	if !strings.Contains(added[0], "application.NewGetUserHandler") {
		t.Errorf("added line = %q", added[0])
	}
}

func TestAddArgumentIsIdempotent(t *testing.T) {
	t.Parallel()

	once, err := astedit.AddArgument([]byte(commented), "warren.Providers", "application.NewGetUserHandler")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	twice, err := astedit.AddArgument(once, "warren.Providers", "application.NewGetUserHandler")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(once) != string(twice) {
		t.Error("running the generator twice changed the file — re-running must be a no-op")
	}
}

func TestAddArgumentCreatesAMissingOption(t *testing.T) {
	t.Parallel()

	// warren.Consumers is absent; adding a consumer must create it inside
	// the NewModule call rather than failing.
	out, err := astedit.AddArgument([]byte(commented), "warren.Consumers", "application.NewSubscription")
	if err != nil {
		t.Fatalf("AddArgument: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "warren.Consumers(") || !strings.Contains(s, "application.NewSubscription") {
		t.Fatalf("the option was not created:\n%s", s)
	}
	// And it is still valid, formatted Go.
	if _, err := astedit.AddArgument(out, "warren.Consumers", "application.NewOther"); err != nil {
		t.Errorf("the created option is not editable: %v", err)
	}
}

func TestAddImport(t *testing.T) {
	t.Parallel()

	out, err := astedit.AddImport([]byte(commented), "example.com/app/internal/modules/user/domain")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	if !strings.Contains(string(out), `"example.com/app/internal/modules/user/domain"`) {
		t.Fatalf("import not added:\n%s", out)
	}
	// Idempotent, like every generator step.
	again, err := astedit.AddImport(out, "example.com/app/internal/modules/user/domain")
	if err != nil {
		t.Fatalf("AddImport again: %v", err)
	}
	if string(again) != string(out) {
		t.Error("adding an existing import changed the file")
	}
}

func TestMissingTargetIsANamedError(t *testing.T) {
	t.Parallel()

	_, err := astedit.AddArgument([]byte("package x\n"), "warren.Providers", "y.Z")
	if err == nil {
		t.Fatal("editing a file with no NewModule call succeeded")
	}
	if !strings.Contains(err.Error(), "warren.NewModule") {
		t.Errorf("the diagnostic does not say what was missing:\n%v", err)
	}
}

func TestInvalidSourceIsRejected(t *testing.T) {
	t.Parallel()

	_, err := astedit.AddArgument([]byte("package x\nfunc ( {"), "warren.Providers", "y.Z")
	if err == nil {
		t.Error("editing unparseable Go succeeded")
	}
}

// diffLines reports lines present in one string and not the other.
func diffLines(before, after string) (added, removed []string) {
	count := map[string]int{}
	for _, l := range strings.Split(before, "\n") {
		count[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(after, "\n") {
		t := strings.TrimSpace(l)
		if count[t] > 0 {
			count[t]--
			continue
		}
		if t != "" {
			added = append(added, l)
		}
	}
	for l, n := range count {
		for range n {
			if l != "" {
				removed = append(removed, l)
			}
		}
	}
	return added, removed
}

// TestEditsTheModuleItWasAskedFor is the mis-wiring test. findCall walks
// the whole file, so a file declaring two modules could have its provider
// spliced into the WRONG one — a mistake that compiles, passes the boot
// test of the module it did not touch, and only surfaces as a container
// resolution failure at runtime.
func TestEditsTheModuleItWasAskedFor(t *testing.T) {
	t.Parallel()

	src := []byte(`package billing

import (
	"sync"

	"github.com/MerseniBilel/warren"

	"example.com/app/internal/modules/billing/infrastructure"
)

var Reporting = sync.OnceValue(func() warren.Module {
	return warren.NewModule("billing.reporting",
		warren.Providers(infrastructure.NewReportStore),
	)
})

var Module = sync.OnceValue(func() warren.Module {
	return warren.NewModule("billing",
		warren.Providers(infrastructure.NewInvoiceRepository),
	)
})
`)
	out, err := astedit.AddArgument(src, "warren.Providers", "infrastructure.NewLedger")
	if err != nil {
		t.Fatalf("AddArgument: %v", err)
	}
	got := string(out)

	// The two declarations, split at the second module's name.
	split := strings.Index(got, `warren.NewModule("billing",`)
	if split < 0 {
		t.Fatalf("the second module declaration is gone:\n%s", got)
	}
	if strings.Contains(got[:split], "NewLedger") {
		t.Errorf("the provider landed in the wrong module:\n%s", got)
	}
	if !strings.Contains(got[split:], "NewLedger") {
		t.Errorf("the provider did not reach the right module:\n%s", got)
	}
}

// TestAddImportKeepsTrailingComments — the offset of an import spec ends at
// the path, not at the end of the line, so a naive splice lands between a
// path and its own comment and the comment ends up documenting the wrong
// import.
func TestAddImportKeepsTrailingComments(t *testing.T) {
	t.Parallel()

	src := []byte(`package main

import (
	"example.com/app/internal/modules/user"
	"example.com/app/internal/platform" // config lives here
)
`)
	out, err := astedit.AddImport(src, "example.com/app/internal/modules/billing")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	got := string(out)
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "config lives here") && !strings.Contains(line, "internal/platform") {
			t.Errorf("the comment was moved onto another import:\n%s", got)
		}
	}
	if !strings.Contains(got, "internal/modules/billing") {
		t.Errorf("the import was not added:\n%s", got)
	}
}

// TestAddImportHandlesASingleUnparenthesizedImport — `import "x"` is legal
// Go and the scaffold is not the only shape this tool meets.
func TestAddImportHandlesASingleUnparenthesizedImport(t *testing.T) {
	t.Parallel()

	src := []byte("package main\n\nimport \"github.com/MerseniBilel/warren\"\n\nfunc main() { _ = warren.New }\n")
	out, err := astedit.AddImport(src, "example.com/app/internal/modules/billing")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	if !strings.Contains(string(out), "internal/modules/billing") {
		t.Errorf("the import was not added:\n%s", out)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", out, 0); err != nil {
		t.Errorf("the result does not parse: %v\n%s", err, out)
	}
}

// TestBlockCommentIsNotStolen — afterLast understood // only, so a /* */
// comment on the same line as the last argument was carried onto the new
// one.
func TestBlockCommentIsNotStolen(t *testing.T) {
	t.Parallel()

	src := []byte(`package m

import "github.com/MerseniBilel/warren"

var Module = warren.NewModule("m",
	warren.Providers(
		app.A, /* keep me with A */
	),
)
`)
	out, err := astedit.AddArgument(src, "warren.Providers", "app.C")
	if err != nil {
		t.Fatalf("AddArgument: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "keep me with A") && !strings.Contains(line, "app.A") {
			t.Errorf("the block comment was moved onto another argument:\n%s", out)
		}
	}
}

// TestCRLFSurvives — format.Source normalises line endings, so a one-line
// wiring edit on a CRLF checkout became a whole-file diff.
func TestCRLFSurvives(t *testing.T) {
	t.Parallel()

	unix := "package m\n\nimport \"github.com/MerseniBilel/warren\"\n\nvar Module = warren.NewModule(\"m\",\n\twarren.Providers(\n\t\tapp.A,\n\t),\n)\n"
	src := []byte(strings.ReplaceAll(unix, "\n", "\r\n"))

	out, err := astedit.AddArgument(src, "warren.Providers", "app.C")
	if err != nil {
		t.Fatalf("AddArgument: %v", err)
	}
	in := strings.Count(string(src), "\r\n")
	got := strings.Count(string(out), "\r\n")
	if got < in {
		t.Errorf("CRLF line endings were stripped: %d in, %d out\n%q", in, got, out)
	}
	if strings.Count(string(out), "\n") != got {
		t.Errorf("the result mixes line endings:\n%q", out)
	}
}
