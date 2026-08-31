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
	"CodeUnsupportedMedia": werrors.CodeUnsupportedMedia,
	"CodeMethodNotAllowed": werrors.CodeMethodNotAllowed,
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

// TestRetryingOnUnauthenticatedDoesNotMentionAStaleWrite — the terminal
// refusal used to emit the CONFLICT reasoning for all five terminal codes,
// and "A stale write is not UNAUTHENTICATED. It is CONTENTION" is a
// non-sequitur: nobody reaches this refusal on an auth code by confusing it
// with optimistic concurrency. They reach it by returning UNAUTHENTICATED for
// a failure to authenticate to something DOWNSTREAM — which AGENT.md's error
// table already answers, and which is what the message has to say.
func TestRetryingOnUnauthenticatedDoesNotMentionAStaleWrite(t *testing.T) {
	t.Parallel()

	for _, code := range []werrors.Code{werrors.CodeUnauthenticated, werrors.CodePermissionDenied} {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()

			msg := recoveredMessage(t, func() {
				_ = app.RetryingOn[string, string](&countingPolicy{max: 3}, code)
			})
			if strings.Contains(msg, "stale write") {
				t.Errorf("the refusal explains %s as a stale write, which it is not:\n%s", code, msg)
			}
			// The sentence the error table carries and this message should
			// have been carrying: the codes describe the caller, not you.
			if !strings.Contains(msg, "UNAVAILABLE") {
				t.Errorf("the refusal does not name the code a downstream auth failure carries:\n%s", msg)
			}
			for _, want := range []string{"downstream", "caller"} {
				if !strings.Contains(strings.ToLower(msg), want) {
					t.Errorf("the refusal does not say %q — it has to name the mistake the reader has probably made:\n%s", want, msg)
				}
			}
		})
	}

	// And the three codes for which the stale-write reading IS the likely
	// mistake must keep it. Correcting one paragraph is not licence to drop it.
	for _, code := range []werrors.Code{werrors.CodeInvalid, werrors.CodeNotFound, werrors.CodeConflict} {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()

			msg := recoveredMessage(t, func() {
				_ = app.RetryingOn[string, string](&countingPolicy{max: 3}, code)
			})
			if !strings.Contains(msg, "A stale write is not "+string(code)) {
				t.Errorf("the refusal lost the CONTENTION reading, which is correct for %s:\n%s", code, msg)
			}
		})
	}
}

// TestRetryingOnPanicMessagesAreGolden — the diagnostics are the product
// (AGENT.md invariant 2), so the text is pinned. Regenerate with
// `go test ./app -run Golden -update`.
//
// One golden per REFUSED code, and the set is derived from errors.Codes()
// rather than listed: the message varies by code now, so a single golden for
// "a terminal code" pinned one fifth of the text and let the other four drift.
// A code that stops panicking must lose its golden, and a code that starts
// panicking gains one — both are failures here until the file is regenerated.
func TestRetryingOnPanicMessagesAreGolden(t *testing.T) {
	t.Parallel()

	for _, code := range werrors.Codes() {
		t.Run(string(code), func(t *testing.T) {
			path := filepath.Join("testdata", "retrying_on_"+strings.ToLower(string(code))+".golden")

			var got string
			var panicked bool
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
						s, ok := r.(string)
						if !ok {
							t.Fatalf("panicked with %T, want a string: %v", r, r)
						}
						got = s
					}
				}()
				_ = app.RetryingOn[string, string](&countingPolicy{max: 3}, code)
			}()

			if !panicked {
				if _, err := os.Stat(path); err == nil {
					t.Errorf("%s is accepted by RetryingOn but still has a golden refusal at %s", code, path)
				}
				return
			}
			compareGolden(t, path, got)
		})
	}

	t.Run("no codes", func(t *testing.T) {
		compareGolden(t, filepath.Join("testdata", "retrying_on_no_codes.golden"),
			recoveredMessage(t, func() { _ = app.RetryingOn[string, string](&countingPolicy{max: 3}) }))
	})
}

// compareGolden pins got against the file at path, rewriting it under -update.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()

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
}

// TestRetryingOnWithANilPolicyNamesRetryingOn — a panic that names the wrong
// function is worse than one that names none: it sends the reader to a call
// site that does not exist in their code. Retrying and RetryingOn share one
// loop, and the loop used to hardcode "Retrying" in the nil-policy guard, so
// everyone who reached it through RetryingOn went looking for the wrong thing.
func TestRetryingOnWithANilPolicyNamesRetryingOn(t *testing.T) {
	t.Parallel()

	msg := recoveredMessage(t, func() {
		_ = app.RetryingOn[string, string](nil, werrors.CodeContention)
	})
	if !strings.Contains(msg, "RetryingOn") {
		t.Errorf("the panic does not name the function that was called:\n%s", msg)
	}
	// "Retrying(" — the call, not the substring inside "RetryingOn".
	if strings.Contains(msg, "Retrying(") || strings.Contains(msg, "Retrying composed") {
		t.Errorf("the panic sends the reader to app.Retrying, which they did not call:\n%s", msg)
	}
}

// TestRetryingWithANilPolicyStillNamesRetrying is the other half: naming the
// caller must not have been done by naming the OTHER caller.
func TestRetryingWithANilPolicyStillNamesRetrying(t *testing.T) {
	t.Parallel()

	msg := recoveredMessage(t, func() { _ = app.Retrying[string, string](nil) })
	if !strings.Contains(msg, "Retrying composed with a nil policy") {
		t.Errorf("the panic no longer names Retrying:\n%s", msg)
	}
	if strings.Contains(msg, "RetryingOn") {
		t.Errorf("the panic names RetryingOn, which the caller did not call:\n%s", msg)
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
