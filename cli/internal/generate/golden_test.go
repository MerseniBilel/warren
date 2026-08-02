package generate_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/generate"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the generators produce")

// TestGolden pins the exact bytes every generator writes.
//
// The compile gate proves generated code is VALID; this proves it is what
// we meant. Templates otherwise drift silently — a comment gets mangled, an
// import moves, a doc paragraph loses a line — and nothing fails, because
// the result still compiles. A reviewer reads the diff on these files and
// sees precisely what changed for a user.
//
// Regenerate with:  go test ./internal/generate -run TestGolden -update
func TestGolden(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// One run of every generator, in the order a user runs them, so the
	// golden module.go also pins the WIRING each one added.
	for _, step := range []struct {
		what string
		run  func() (string, error)
	}{
		{"module", func() (string, error) {
			return generate.Module(generate.Options{Dir: dir, Name: "billing"})
		}},
		{"entity", func() (string, error) {
			return generate.Entity(generate.Options{Dir: dir, Module: "billing", Name: "Invoice"})
		}},
		{"repository", func() (string, error) {
			return generate.Repository(generate.Options{Dir: dir, Module: "billing", Name: "Invoice"})
		}},
		{"command", func() (string, error) {
			return generate.Command(generate.Options{Dir: dir, Module: "billing", Name: "VoidInvoice"})
		}},
		{"consumer", func() (string, error) {
			return generate.Consumer(generate.Options{Dir: dir, Module: "billing", Name: "PaymentReceived"})
		}},
	} {
		if _, err := step.run(); err != nil {
			t.Fatalf("g %s: %v", step.what, err)
		}
	}

	// module.go is last on purpose: it is the file every generator edits,
	// and the one where a splicing regression shows up.
	for _, rel := range []string{
		"internal/modules/billing/domain/invoice.go",
		"internal/modules/billing/infrastructure/invoice_repository.go",
		"internal/modules/billing/application/void_invoice.go",
		"internal/modules/billing/application/void_invoice_test.go",
		"internal/modules/billing/application/on_payment_received.go",
		"internal/modules/billing/on_payment_received_subscription.go",
		"internal/modules/billing/module_test.go",
		"internal/modules/billing/module.go",
	} {
		golden(t, dir, rel)
	}
}

// golden compares one generated file with its recorded form.
func golden(t *testing.T, dir, rel string) {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading generated %s: %v", rel, err)
	}
	// A flat name: the golden files are meant to be read side by side in a
	// review, not navigated as a tree.
	path := filepath.Join("testdata", "golden", strings.ReplaceAll(rel, "/", "__")+".golden")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file for %s — run:\n\n    go test ./internal/generate -run TestGolden -update\n\n%v", rel, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match its golden file.\n\n%s\n\n"+
			"If the change is intended, re-record it with:\n\n"+
			"    go test ./internal/generate -run TestGolden -update\n",
			rel, firstDifference(string(want), string(got)))
	}
}

// firstDifference reports the first line that differs, with a little
// context. A whole-file dump of two near-identical files tells a reviewer
// nothing.
func firstDifference(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := at(wantLines, i), at(gotLines, i)
		if w == g {
			continue
		}
		var b strings.Builder
		for c := max(0, i-2); c < i; c++ {
			b.WriteString("      " + at(wantLines, c) + "\n")
		}
		b.WriteString("  -   " + w + "\n")
		b.WriteString("  +   " + g)
		return "  first difference, line " + itoa(i+1) + ":\n\n" + b.String()
	}
	return "  the files differ only in trailing content"
}

func at(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}
