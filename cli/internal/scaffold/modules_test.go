package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFrameworkModulesCoversTheWorkspace holds frameworkModules to the claim
// its own comment makes: EVERY Warren module, not only the ones a scaffold
// requires today.
//
// The claim was enforced by nothing, so it was false twice. A field test
// caught validate/playground missing; openapi was then missing from the day
// it was written until field test #14 found it the same way — a user wired
// openapi.Module(), had no replace for it, and `go get` resolved the module
// from GITHUB while the other six came from their checkout. Two versions of
// the framework in one build, silently, which is precisely the failure the
// comment above the list describes.
//
// go.work is the source of truth because it is the file that would also have
// to change: a module the workspace does not use is not a module of this
// repository. The CLI is excluded deliberately — it is a build-time tool and
// never appears in a service's go.mod, which is why it is absent from the
// list it is testing.
func TestFrameworkModulesCoversTheWorkspace(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	work, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		t.Skipf("no go.work to compare against: %v", err)
	}

	listed := map[string]bool{}
	for _, m := range frameworkModules {
		listed[m] = true
	}

	var missing []string
	for _, use := range workspaceUses(string(work)) {
		if use == "/cli" {
			continue
		}
		if !listed[use] {
			missing = append(missing, use)
		}
	}
	if len(missing) > 0 {
		t.Errorf("go.work uses modules frameworkModules omits: %v\n"+
			"a --framework scaffold writes no replace for them, so `go get` resolves "+
			"them from the proxy while the rest come from the checkout — two versions "+
			"of the framework in one build", missing)
	}

	// The other direction: a replace for a module that does not exist is not
	// inert the way an unrequired one is. `go mod tidy` fails on it.
	uses := map[string]bool{}
	for _, use := range workspaceUses(string(work)) {
		uses[use] = true
	}
	for _, m := range frameworkModules {
		if !uses[m] {
			t.Errorf("frameworkModules lists %q, which go.work does not use — "+
				"a replace pointing at a directory with no go.mod fails `go mod tidy`", m)
		}
	}
}

// workspaceUses returns each `use` path in a go.work as a module suffix: "."
// becomes "" and "./transport/http" becomes "/transport/http", matching the
// shape frameworkModules stores.
func workspaceUses(work string) []string {
	var out []string
	inBlock := false
	for line := range strings.SplitSeq(work, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "use ("):
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "":
			out = append(out, suffixOf(line))
		case strings.HasPrefix(line, "use "):
			out = append(out, suffixOf(strings.TrimSpace(strings.TrimPrefix(line, "use "))))
		}
	}
	return out
}

func suffixOf(use string) string {
	use = strings.Trim(use, `"`)
	if use == "." {
		return ""
	}
	return strings.TrimPrefix(use, ".")
}

// repoRoot walks up from the working directory to the checkout containing
// go.work. The test binary runs in the package directory, three levels down.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("no go.work above the working directory; not a checkout")
	return ""
}

// TestAScaffoldCanUseTheValidateVocabularyItIsTaught — field test #15's
// scaffold gap, and the one point it took off time-to-first-endpoint.
//
// GETTING_STARTED §3 introduces `validate:"required"` and §9 shows `min=`,
// `email` and `oneof` as ordinary vocabulary. Core understands `required` and
// refuses every other tag rather than ignoring it — correctly — so the first
// interesting tag a user writes fails the boot, on line one of every real DTO,
// in a project the scaffold had just told them was ready. The scaffold already
// knew this would happen.
func TestAScaffoldCanUseTheValidateVocabularyItIsTaught(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := New(Options{
		Dir: dir, Name: "shop", ModulePath: "example.com/shop",
		Version: DefaultVersion,
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gomod), "warren/validate/playground") {
		t.Errorf("the scaffold does not require an implementation of the vocabulary it teaches:\n%s", gomod)
	}

	main, err := os.ReadFile(filepath.Join(dir, "cmd/shop/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Requiring it and not binding it is worse than neither: the dependency
	// is paid for and the boot still fails.
	for _, want := range []string{"playground.New()", "app.Validator("} {
		if !strings.Contains(string(main), want) {
			t.Errorf("main.go does not bind the validator (%q):\n%s", want, main)
		}
	}
}
