package openapi

import (
	"fmt"
	"strings"

	"github.com/MerseniBilel/warren/app"
)

// Option configures Module and Emit.
type Option struct{ apply func(*config) }

type config struct {
	title       string
	version     string
	description string
	servers     []ServerInfo
	specPath    string
	docsPath    string
	guards      []app.AuthorizationPolicy
	strict      bool
}

func defaults() config {
	return config{
		title:    "API",
		version:  "0.0.0",
		specPath: "/openapi.json",
	}
}

// Title sets the document's title.
func Title(s string) Option { return Option{apply: func(c *config) { c.title = s }} }

// Version sets the API's version — the service's, not Warren's. A document
// that reports the framework's version tells a client nothing it can act on.
func Version(s string) Option { return Option{apply: func(c *config) { c.version = s }} }

// Description sets the document's description.
func Description(s string) Option { return Option{apply: func(c *config) { c.description = s }} }

// Server appends an entry to `servers`. It is repeatable.
//
// Nothing is derived. The route table knows no scheme, port or base path, and
// http://localhost:8080 baked into every generated client is worse than an
// absent servers list — a client author who sees nothing asks; one who sees a
// wrong URL ships it.
func Server(url, description string) Option {
	return Option{apply: func(c *config) {
		c.servers = append(c.servers, ServerInfo{URL: url, Description: description})
	}}
}

// SpecPath serves the document somewhere other than /openapi.json.
func SpecPath(p string) Option { return Option{apply: func(c *config) { c.specPath = p }} }

// DocsPath reserves a path for a rendered UI. Empty disables it.
//
// NOTHING IS SERVED THERE YET — the HTML bundle is a dependency decision that
// has not been taken (warren.md §9 requires an audit before any third-party
// asset enters the tree). The option exists so the path is claimed and the
// emitter knows not to describe it.
func DocsPath(p string) Option { return Option{apply: func(c *config) { c.docsPath = p }} }

// Guard attaches an authorization policy to the document route. An internal
// API's shape is not public, and this is how a service says so.
func Guard(p app.AuthorizationPolicy) Option {
	return Option{apply: func(c *config) { c.guards = append(c.guards, p) }}
}

// Strict turns every refusal into a boot failure.
//
// Off by default, deliberately: a refusal is information, and a service should
// not fail to start because one route uses the escape hatch. On, it is the
// gate a team turns on once its document is complete and wants to keep it
// that way — the same shape as a linter you adopt after you are clean.
func Strict() Option { return Option{apply: func(c *config) { c.strict = true }} }

// errStrict renders the refusals as one boot failure.
func errStrict(refusals []Refusal) error {
	var b strings.Builder
	fmt.Fprintf(&b, "✗ openapi.Strict(): %d route(s) could not be fully described\n\n", len(refusals))
	for _, r := range refusals {
		fmt.Fprintf(&b, "    %s\n      %s\n", r.Route, r.Reason)
		if r.Type != "" {
			fmt.Fprintf(&b, "      type: %s\n", r.Type)
		}
	}
	b.WriteString("\n  Each of these is EMITTED — the document lists the route and says it\n")
	b.WriteString("  could not describe it. Strict() turns that admission into a boot\n")
	b.WriteString("  failure, so drop Strict(), give the route a typed handler, or accept\n")
	b.WriteString("  the refusal by keeping it off.")
	return fmt.Errorf("%s", b.String())
}
