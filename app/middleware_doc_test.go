package app_test

import (
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
)

// The doc comment on RetryingOn is executable advice: a reader copies an
// example into a Chain and boots. Five of the eight codes are refused at
// composition, so an example naming one of them is not a typo — it is a
// documented boot panic, and the doc block carried three of them until
// 2026-08-09.
//
// These tests read the SOURCE. Parsing the comment is the only way to assert
// a property of the comment; a hand-maintained list of "the examples say X"
// would drift from the comment on the first edit, which is the failure being
// fixed.

// codeByIdent maps the Go identifier a doc example spells to the Code it
// names. It is exhaustive by assertion below, so a ninth code cannot be added
// to errors without this test noticing.
var codeByIdent = map[string]werrors.Code{
	"CodeInvalid":          werrors.CodeInvalid,
	"CodeNotFound":         werrors.CodeNotFound,
	"CodeConflict":         werrors.CodeConflict,
	"CodeContention":       werrors.CodeContention,
	"CodeUnauthenticated":  werrors.CodeUnauthenticated,
	"CodePermissionDenied": werrors.CodePermissionDenied,
	"CodeUnavailable":      werrors.CodeUnavailable,
	"CodeInternal":         werrors.CodeInternal,
}

// docOf returns the doc comment of the named top-level function in the given
// file of this package.
func docOf(t *testing.T, file, fn string) string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Recv == nil {
			if fd.Doc == nil {
				t.Fatalf("%s has no doc comment", fn)
			}
			return fd.Doc.Text()
		}
	}
	t.Fatalf("no func %s in %s", fn, file)
	return ""
}

// codeExamples returns the Code identifiers named inside the INDENTED example
// blocks of a doc comment — godoc code blocks, the lines a reader copies.
// Prose is deliberately excluded: prose has to be able to say "a stale write
// is not CodeConflict", and saying so is not an instruction to compose it.
func codeExamples(t *testing.T, doc string) (codes []werrors.Code, blocks int) {
	t.Helper()

	ident := regexp.MustCompile(`\bCode[A-Z][A-Za-z]*\b`)
	var p comment.Parser
	for _, block := range p.Parse(doc).Content {
		code, ok := block.(*comment.Code)
		if !ok {
			continue
		}
		blocks++
		for _, name := range ident.FindAllString(code.Text, -1) {
			c, known := codeByIdent[name]
			if !known {
				t.Errorf("example names %s, which is not a code in warren/errors:\n%s", name, code.Text)
				continue
			}
			codes = append(codes, c)
		}
	}
	return codes, blocks
}

// TestErrorCodeIdentifiersAreExhaustive keeps codeByIdent honest. Without it
// a ninth code would simply never be recognised in an example, and the guard
// below would pass by not looking.
func TestErrorCodeIdentifiersAreExhaustive(t *testing.T) {
	t.Parallel()

	if got, want := len(codeByIdent), len(werrors.Codes()); got != want {
		t.Fatalf("codeByIdent has %d entries, errors.Codes() has %d — add the new code here", got, want)
	}
	for _, c := range werrors.Codes() {
		found := false
		for _, mapped := range codeByIdent {
			if mapped == c {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not reachable through codeByIdent", c)
		}
	}
}

// TestRetryingOnDocExamplesCompose is the guard the stale doc block needed:
// every code the examples name is fed to the real RetryingOn, and the real
// guard decides. No second copy of the terminal list lives here — a
// duplicated list is how the comment and the code drifted apart in the first
// place.
func TestRetryingOnDocExamplesCompose(t *testing.T) {
	t.Parallel()

	doc := docOf(t, "middleware.go", "RetryingOn")
	codes, blocks := codeExamples(t, doc)

	// A doc block with no examples would pass vacuously.
	if blocks < 2 {
		t.Errorf("RetryingOn's doc has %d example blocks; it documents a code list and needs examples", blocks)
	}
	if len(codes) == 0 {
		t.Fatal("RetryingOn's examples name no error code at all")
	}

	for _, c := range codes {
		t.Run(string(c), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RetryingOn's doc shows %s, and composing it panics:\n%v", c, r)
				}
			}()
			_ = app.RetryingOn[string, string](&countingPolicy{max: 3}, c)
		})
	}
}

// TestRetryingOnDocExamplesRun executes the compositions the doc shows, so an
// example that is legal but no longer type-checks against the signature is
// caught too. The list mirrors the doc's example blocks by hand — this half
// cannot be derived, and the test above is what stops the two diverging on
// the part that matters.
func TestRetryingOnDocExamplesRun(t *testing.T) {
	t.Parallel()

	policy := &countingPolicy{max: 3}
	_ = app.Chain(echo(),
		app.RetryingOn[string, string](policy, werrors.CodeContention),
		app.Transactional[string, string](&recordingUoW{}),
	)
	_ = app.RetryingOn[string, string](policy, werrors.CodeInternal, werrors.CodeUnavailable)
	_ = app.RetryingOn[string, string](policy, werrors.CodeContention, werrors.CodeUnavailable)
}

// TestRetryingOnPanicsEnumerateTheLegalSet — a user reading the signature has
// no way to learn that five of the eight codes are refused except by
// triggering the panic, so the panic has to say. Both of them: the terminal
// refusal and the empty-set refusal.
func TestRetryingOnPanicsEnumerateTheLegalSet(t *testing.T) {
	t.Parallel()

	legal := []string{"CONTENTION", "UNAVAILABLE", "INTERNAL"}
	for _, tc := range []struct {
		name string
		call func()
	}{
		{"terminal code", func() { _ = app.RetryingOn[string, string](&countingPolicy{max: 3}, werrors.CodeConflict) }},
		{"no codes", func() { _ = app.RetryingOn[string, string](&countingPolicy{max: 3}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := recoveredMessage(t, tc.call)
			for _, code := range legal {
				if !strings.Contains(msg, code) {
					t.Errorf("the panic does not name %s as legal:\n%s", code, msg)
				}
			}
		})
	}
}

// TestRetryingOnPanicMessagesAreGolden — the diagnostics are the product
// (AGENT.md invariant 2), so the text is pinned. Regenerate with
// `go test ./app -run Golden -update`.
func TestRetryingOnPanicMessagesAreGolden(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		golden string
		call   func()
	}{
		{
			"terminal code",
			"retrying_on_terminal.golden",
			func() { _ = app.RetryingOn[string, string](&countingPolicy{max: 3}, werrors.CodeConflict) },
		},
		{
			"no codes",
			"retrying_on_no_codes.golden",
			func() { _ = app.RetryingOn[string, string](&countingPolicy{max: 3}) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveredMessage(t, tc.call)
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("writing golden file: %v", err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden file: %v", err)
			}
			if got != string(want) {
				t.Errorf("panic text changed:\ngot:\n%s\n\nwant:\n%s", got, want)
			}
		})
	}
}

// recoveredMessage runs call and returns the string it panicked with.
func recoveredMessage(t *testing.T, call func()) (msg string) {
	t.Helper()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the composition was accepted; it must panic")
		}
		s, ok := r.(string)
		if !ok {
			t.Fatalf("panicked with %T, want a string: %v", r, r)
		}
		msg = s
	}()
	call()
	return ""
}
