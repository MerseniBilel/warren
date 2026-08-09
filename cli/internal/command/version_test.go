package command_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionMatchesTheBuild is the test a hardcoded constant cannot pass
// twice.
//
// `warren version` printed "warren v0.1.0 (framework v0.1.0)" out of a binary
// the go command had stamped v0.2.0: the constant was right once, nobody
// edited it when the tag moved, and nothing anywhere noticed. So this builds
// the CLI the way a user's `go install` does, then compares what the binary
// PRINTS with what the go command STAMPED into it. The two cannot disagree
// without failing here, whatever the tag is next time.
func TestVersionMatchesTheBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI; skipped under -short")
	}
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "warren")
	build := exec.Command("go", "build", "-o", bin, "./cmd/warren")
	build.Dir = filepath.Join("..", "..") // the cli module root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/warren: %v\n%s", err, out)
	}

	stamped := stampedVersion(t, bin)

	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("warren version: %v\n%s", err, out)
	}
	printed := strings.TrimSpace(string(out))
	if !strings.Contains(printed, "warren "+stamped) {
		t.Errorf("`warren version` printed %q; the go command stamped this binary %s",
			printed, stamped)
	}
}

// stampedVersion reads the main module's version back out of a built binary.
// It is the same string runtime/debug.ReadBuildInfo hands the program itself,
// read from the outside so the test does not depend on the code under test to
// tell the truth about it.
func stampedVersion(t *testing.T, bin string) string {
	t.Helper()

	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "mod" && f[1] == "github.com/MerseniBilel/warren/cli" {
			return f[2]
		}
	}
	t.Fatalf("no `mod github.com/MerseniBilel/warren/cli` line in:\n%s", out)
	return ""
}
