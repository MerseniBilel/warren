package command

import (
	"regexp"
	"runtime/debug"

	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// devel is what a binary built from a checkout reports for its own version.
// It is the go command's own word for it, and printing it verbatim is the
// honest answer: this binary is not a release, and saying otherwise is how
// `warren version` came to claim v0.1.0 out of a v0.2.0 build.
const devel = "(devel)"

// Version is the CLI's own version, read from the build rather than written
// down. `go install …@v0.2.0` stamps v0.2.0; a `go build` from a clean
// checkout at a tag stamps the tag; anything else stamps a pseudo-version, a
// "+dirty" suffix, or "(devel)" — and all of those are reported as they are.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return devel
	}
	return info.Main.Version
}

// FrameworkVersion is the version of the framework a scaffold pins.
//
// It is the CLI's own version whenever that names a release: the modules are
// tagged in lockstep, so a `warren` you installed at vX.Y.Z generates code
// against the framework at vX.Y.Z and the two cannot disagree.
//
// The other builds have no release to offer. A pseudo-version belongs to the
// CLI module's tag history and names nothing the proxy serves for the core
// module; "(devel)" and "+dirty" name nothing at all. Writing any of them
// into a user's go.mod produces a project that cannot resolve — the exact
// failure the published tags exist to end — so those fall back to
// scaffold.DefaultVersion, which a test checks against git.
func FrameworkVersion() string {
	if v := Version(); isRelease(v) {
		return v
	}
	return scaffold.DefaultVersion
}

var (
	// releaseTag matches vMAJOR.MINOR.PATCH with an optional prerelease. A
	// "+dirty" or any other build metadata fails it, which is the point.
	releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// pseudoVersion matches the go command's own <timestamp>-<revision>
	// suffix. It parses as a prerelease, so releaseTag alone would accept
	// it. The separator before the timestamp is a dash in the untagged form
	// (v0.0.0-20060102150405-abcdefabcdef) and a dot once a base tag exists
	// (v1.2.4-0.20060102150405-abcdefabcdef), so both are matched — a
	// dash-only pattern accepted every build made between two tags, which
	// is every build a Warren contributor makes.
	pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}$`)
)

// isRelease reports whether v is a version the module proxy can serve.
func isRelease(v string) bool {
	return releaseTag.MatchString(v) && !pseudoVersion.MatchString(v)
}
