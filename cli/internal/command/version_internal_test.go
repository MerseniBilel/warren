package command

import "testing"

// TestIsRelease is the rule that decides whether this binary's own version is
// safe to write into a user's go.mod. Only a version the module proxy can
// serve is: everything else falls back to scaffold.DefaultVersion.
func TestIsRelease(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version string
		release bool
		why     string
	}{
		{"v0.2.0", true, "a plain tag, which is what `go install …@v0.2.0` stamps"},
		{"v1.10.3", true, "two-digit components are still a tag"},
		{"v0.3.0-rc.1", true, "a prerelease is tagged and served"},
		{devel, false, "a checkout build has no release to name"},
		{"", false, "an empty version names nothing"},
		{"v0.2.0+dirty", false, "uncommitted changes; the proxy serves no such thing"},
		{"v0.2.1-0.20260809210721-bb63171058e9", false,
			"a pseudo-version belongs to the CLI module's tags, not the core module's"},
		{"v0.0.0-20260809210721-bb63171058e9", false, "the untagged pseudo-version shape"},
		{"0.2.0", false, "no v prefix is not a Go module version"},
		{"v0.2", false, "a two-component version is not a tag Warren cuts"},
	} {
		if got := isRelease(tc.version); got != tc.release {
			t.Errorf("isRelease(%q) = %v, want %v — %s", tc.version, got, tc.release, tc.why)
		}
	}
}
