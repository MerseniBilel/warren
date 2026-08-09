package scaffold

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestDefaultVersionIsTagged is the guard on the one constant left in the
// version path.
//
// Everything else derives from the build: `warren version` reports what the
// go command stamped, and a scaffold pins the CLI's own released version.
// DefaultVersion covers the case that has no build to derive from — a
// `go build` from a checkout, whose pseudo-version the proxy does not serve —
// and a constant is exactly the thing that was wrong before: v0.1.0 stayed
// written down for 134 commits while the tags moved to v0.2.0, and the only
// symptom was that nobody could resolve a scaffold.
//
// So the constant is checked against git, in three directions:
//
//   - every framework module must be tagged at DefaultVersion — the go.mod a
//     scaffold writes pins that version for all six, and one missing tag is a
//     project that does not resolve;
//   - so must the CLI, because an INSTALLED release pins the framework at its
//     own version rather than at this constant;
//   - and no tag may be NEWER than it — that is the drift that actually
//     happened.
//
// The consequence for a release: bump this constant and create the tags in
// the same operation. A commit that does one without the other is a commit
// whose CLI scaffolds a version the proxy will not serve, and it fails here
// rather than in a user's terminal.
func TestDefaultVersionIsTagged(t *testing.T) {
	t.Parallel()

	tags := repoTags(t)
	if len(tags) == 0 {
		// A shallow clone, or a build from the module cache. CI checks out
		// with fetch-depth 0 precisely so this does not silently skip
		// there.
		t.Skip("no git tags visible; nothing to check the constant against")
	}

	for _, m := range frameworkModules {
		want := DefaultVersion
		if m != "" {
			want = strings.TrimPrefix(m, "/") + "/" + DefaultVersion
		}
		if !tags[want] {
			t.Errorf("scaffold.DefaultVersion is %s, but there is no %s tag — "+
				"a go.mod pinning it does not resolve", DefaultVersion, want)
		}
	}

	// The CLI is not in a user's go.mod, so it is not in frameworkModules —
	// but it is the module `go install …@latest` resolves, and an installed
	// release pins the framework at ITS OWN version rather than at
	// DefaultVersion. Tag cli/v0.3.0 without a core v0.3.0 and every scaffold
	// that binary writes is unresolvable, with nothing above catching it.
	if !tags["cli/"+DefaultVersion] {
		t.Errorf("there is no cli/%s tag — an installed CLI pins the framework at its "+
			"own version, so the two are released together or not at all", DefaultVersion)
	}

	newest, from := "", ""
	for tag := range tags {
		base, ok := strings.CutPrefix(tag, "cli/")
		if !ok {
			base = tag
			if strings.Contains(tag, "/") {
				continue // an adapter tag; the loop above covers those
			}
		}
		if _, isSemver := semver(base); !isSemver {
			continue
		}
		if newest == "" || less(newest, base) {
			newest, from = base, tag
		}
	}
	if newest != "" && less(DefaultVersion, newest) {
		t.Errorf("the newest release tag is %s but scaffold.DefaultVersion is still %s — "+
			"a scaffold built from a checkout pins a release that is not the current one",
			from, DefaultVersion)
	}
}

// repoTags reads the tags of the repository this package lives in. It returns
// nothing rather than failing when git is unavailable: the test's job is to
// catch drift, not to require a VCS.
func repoTags(t *testing.T) map[string]bool {
	t.Helper()

	out, err := exec.Command("git", "tag", "--list").Output()
	if err != nil {
		return nil
	}
	tags := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if tag := strings.TrimSpace(line); tag != "" {
			tags[tag] = true
		}
	}
	return tags
}

// less orders two vMAJOR.MINOR.PATCH tags. Prereleases are not modelled:
// Warren has never cut one, and a wrong answer here would be a test that
// fails on a tag nobody made.
func less(a, b string) bool {
	x, okA := semver(a)
	y, okB := semver(b)
	if !okA || !okB {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func semver(tag string) ([3]int, bool) {
	var out [3]int
	rest, ok := strings.CutPrefix(tag, "v")
	if !ok {
		return out, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
