package astedit_test

import (
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
