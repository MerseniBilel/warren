// Package arch is the architecture linter: it enforces the rules Warren is
// built on, on any Go project, using nothing but the import graph.
//
// It reads imports SYNTACTICALLY, so it works on a project that does not
// compile — which is exactly when it is most needed, because the fix for a
// layer violation usually breaks the build first.
package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// RuleSet selects which rules to apply.
type RuleSet uint8

const (
	// Layers is the rule set for any project: the four-layer rule, plus the
	// cross-module rule that makes extraction a rewiring rather than a
	// rewrite.
	Layers RuleSet = 1 << iota
	// Rings is Warren's own repository: kernel, contracts, adapters, tooling.
	Rings
)

// Options configure a check.
type Options struct {
	Rules RuleSet
}

// Violation is one broken rule.
type Violation struct {
	File          string // relative to the project root
	Line          int
	Package       string
	Layer         string
	Imported      string
	ImportedLayer string
	Rule          string // the rule's short name, for the explanation
	// Via is the chain from Package to the offending import, for a rule
	// broken through another package rather than directly. Naming only the
	// endpoints sends the reader looking in a file that is innocent.
	Via []string
}

// Report is the result of a check.
type Report struct {
	Violations []Violation
	Packages   int
}

// layerOf reports the layer a package path belongs to. The LAST recognised
// segment wins, so internal/modules/user/domain is domain even though the
// path contains "modules". A package with no such segment is unlayered and
// exempt — configuration would be a barrier to a linter's first run.
func layerOf(pkgPath string) string {
	layer := ""
	for _, seg := range strings.Split(pkgPath, "/") {
		switch seg {
		case "domain", "application", "infrastructure", "interfaces":
			layer = seg
		}
	}
	return layer
}

// featureOf reports which feature module a package belongs to — the segment
// after "modules" — or "" when it is not inside one.
func featureOf(pkgPath string) string {
	segs := strings.Split(pkgPath, "/")
	for i, seg := range segs {
		if seg == "modules" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}

// forbidden lists, per layer, the layers it may not import.
var forbidden = map[string][]string{
	"domain":         {"application", "infrastructure", "interfaces"},
	"application":    {"infrastructure", "interfaces"},
	"interfaces":     {"infrastructure"},
	"infrastructure": {},
}

// Check walks the module rooted at dir and reports every violation.
func Check(dir string, opts Options) (*Report, error) {
	modPath, err := modulePath(dir)
	if err != nil {
		return nil, err
	}

	report := &Report{}
	seen := map[string]bool{}
	// The import graph of the project's OWN packages, for the rules a single
	// file cannot answer. A handler that imports net/http is caught by
	// reading one file; a handler that imports a helper that imports
	// net/http is not, and that is the shape the documentation walks people
	// into — so the graph is built as the walk goes and queried afterwards.
	graph := map[string][]string{}
	// Where each package first imports each thing, so a transitive report
	// can point at a real line rather than at a package name.
	site := map[string]Violation{}

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == dir {
				return nil // never skip the root, whatever it is called
			}
			name := d.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		pkgPath := modPath
		if pkgDir != "." {
			pkgPath = modPath + "/" + pkgDir
		}
		if !seen[pkgPath] {
			seen[pkgPath] = true
			report.Packages++
		}

		// module.go is the one file permitted to see all four layers: it is
		// where a feature is wired together.
		if filepath.Base(rel) == "module.go" {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			// A file that will not parse is skipped, not fatal: the check
			// exists to run on broken code.
			return nil
		}

		layer := layerOf(pkgPath)
		feature := featureOf(pkgPath)

		for _, spec := range f.Imports {
			imported, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			pos := fset.Position(spec.Pos())

			// Record every edge, in-module or foreign, before any rule
			// decides about it: the transitive pass needs the foreign leaves
			// as much as the internal hops.
			if !slices.Contains(graph[pkgPath], imported) {
				graph[pkgPath] = append(graph[pkgPath], imported)
			}
			if _, ok := site[pkgPath+" "+imported]; !ok {
				site[pkgPath+" "+imported] = Violation{
					File: rel, Line: pos.Line, Package: pkgPath,
					Layer: layer, Imported: imported,
				}
			}

			// The transport rule is the one that looks OUTSIDE the project:
			// what a handler must not import is the framework's transport
			// port, a driver, or net/http — none of which carry the project's
			// module path. Skipping every foreign import, as the layer rules
			// do, is why invariant 5 went unchecked.
			if opts.Rules&Layers != 0 && isTransportPackage(imported) && slices.Contains(handlerLayers, layer) {
				report.Violations = append(report.Violations, Violation{
					File: rel, Line: pos.Line, Package: pkgPath, Layer: layer,
					Imported: imported, Rule: "transport",
				})
				continue
			}
			if !strings.HasPrefix(imported, modPath) {
				continue // otherwise third party and stdlib are not this rule's business
			}

			if opts.Rules&Layers != 0 {
				if l := layerOf(imported); layer != "" && l != "" && slices.Contains(forbidden[layer], l) {
					report.Violations = append(report.Violations, Violation{
						File: rel, Line: pos.Line, Package: pkgPath, Layer: layer,
						Imported: imported, ImportedLayer: l, Rule: "layer",
					})
					continue
				}
				if other := featureOf(imported); feature != "" && other != "" && other != feature {
					report.Violations = append(report.Violations, Violation{
						File: rel, Line: pos.Line, Package: pkgPath, Layer: layer,
						Imported: imported, ImportedLayer: layerOf(imported), Rule: "cross-module",
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if opts.Rules&Layers != 0 {
		findTransportChains(report, graph, site, modPath)
	}

	sort.Slice(report.Violations, func(i, j int) bool {
		a, b := report.Violations[i], report.Violations[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return report, nil
}

// findTransportChains reports a handler-layer package that reaches a
// transport package THROUGH another package in the same module.
//
// The direct rule reads one file and is easy to satisfy by accident: move
// the import into a helper and the check goes quiet. That is not a corner
// case — GETTING_STARTED tells you to write your own edge middleware with
// whttp.WriteError, and the obvious factoring puts it next to the tenant
// reader that the application layer needs, so a project following the
// documentation reaches net/http from its handlers and lints clean. A field
// test's application layer did, across 19 packages, and only `go list -deps`
// showed it.
//
// Only the project's OWN packages are traversed. A third-party helper that
// happens to import net/http is not something the reader can restructure,
// and a linter that reports it is one they switch off.
func findTransportChains(report *Report, graph map[string][]string, site map[string]Violation, modPath string) {
	// A package already reported directly is not reported again: the reader
	// has the import, and the chain to it adds nothing.
	direct := map[string]bool{}
	for _, v := range report.Violations {
		if v.Rule == "transport" {
			direct[v.Package] = true
		}
	}

	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	for _, pkg := range pkgs {
		if direct[pkg] || !slices.Contains(handlerLayers, layerOf(pkg)) {
			continue
		}
		chain, offender := reachesTransport(graph, pkg, modPath)
		if offender == "" {
			continue
		}
		// The line reported is the handler's own import of the next hop:
		// that is the edge the reader owns and the one they will change.
		v := site[pkg+" "+chain[1]]
		v.Rule = "transport-chain"
		v.Imported = offender
		v.Via = chain
		report.Violations = append(report.Violations, v)
	}
}

// reachesTransport returns the shortest chain from pkg to a transport
// package, and that package. The chain starts at pkg and ends at the last
// in-module package before the offending import.
//
// Breadth-first, so the reported chain is the shortest one — a reader given
// a six-hop path when a two-hop one exists will not believe the tool.
func reachesTransport(graph map[string][]string, pkg, modPath string) ([]string, string) {
	type node struct {
		pkg  string
		path []string
	}
	seen := map[string]bool{pkg: true}
	queue := []node{{pkg: pkg, path: []string{pkg}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, imp := range graph[cur.pkg] {
			if isTransportPackage(imp) {
				return cur.path, imp
			}
			// Foreign packages are leaves: their imports are not this
			// project's to restructure, and they are not in the graph.
			if !strings.HasPrefix(imp, modPath) || seen[imp] {
				continue
			}
			seen[imp] = true
			queue = append(queue, node{pkg: imp, path: append(append([]string{}, cur.path...), imp)})
		}
	}
	return nil, ""
}

// handlerLayers are the layers a use case lives in. The controller is
// unlayered — it sits at the module root, and it is precisely where a use
// case is allowed to meet a protocol — so it is exempt by construction
// rather than by exception.
//
// infrastructure is exempt too, and deliberately: an adapter calling a
// third-party API over net/http is the ordinary reason it exists.
var handlerLayers = []string{"domain", "application"}

// transportPackages are the import prefixes that make a package a transport
// concern. The list is explicit rather than heuristic: a linter that guesses
// is one people switch off.
var transportPackages = []string{
	"net/http",
	"net/http/httputil",
	"github.com/MerseniBilel/warren/transport",
	"github.com/go-chi/chi",
	"github.com/gin-gonic/gin",
	"github.com/labstack/echo",
	"github.com/gorilla/mux",
	"github.com/gofiber/fiber",
	"google.golang.org/grpc",
	"github.com/gorilla/websocket",
}

// isTransportPackage reports whether path is one of them, or lives beneath
// one — warren/transport/http is as much a transport package as its parent.
func isTransportPackage(path string) bool {
	for _, p := range transportPackages {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// modulePath reads the module path from go.mod.
func modulePath(dir string) (string, error) {
	src, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", diagnostic(fmt.Sprintf(
			"✗ not a Go module\n\n    %s has no go.mod.\n\n"+
				"  Run warren lint arch from the root of a Go module.", dir))
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", diagnostic("✗ go.mod declares no module path")
}

// String renders the report the way every Warren diagnostic reads: what
// broke, where, and what to do about it.
func (r *Report) String() string {
	if len(r.Violations) == 0 {
		return fmt.Sprintf("No violations in %d packages.\n", r.Packages)
	}
	var b strings.Builder
	for _, v := range r.Violations {
		switch v.Rule {
		case "layer":
			fmt.Fprintf(&b, "✗ layer violation\n\n    %s:%d\n      package %s          (layer: %s)\n        imports %s (layer: %s)\n\n%s\n\n",
				v.File, v.Line, v.Package, v.Layer, v.Imported, v.ImportedLayer, explain(v))
		case "cross-module":
			fmt.Fprintf(&b, "✗ cross-module import\n\n    %s:%d\n      package %s\n        imports %s\n\n%s\n\n",
				v.File, v.Line, v.Package, v.Imported, explain(v))
		case "transport-chain":
			what := "handler"
			if v.Layer == "domain" {
				what = "domain"
			}
			fmt.Fprintf(&b, "✗ the %s reaches a transport package\n\n    %s:%d\n      package %s          (layer: %s)\n%s        imports %s\n\n%s\n\n",
				what, v.File, v.Line, v.Package, v.Layer, chainOf(v), v.Imported, explain(v))
		case "transport":
			// One rule, two layers, and they are not the same mistake. A use
			// case in application/ has routing that belongs in controller.go;
			// a type in domain/ has none, and being told to move it is how a
			// reader decides the linter has not understood their code.
			headline := "handler imports a transport package"
			if v.Layer == "domain" {
				headline = "the domain imports a transport package"
			}
			fmt.Fprintf(&b, "✗ %s\n\n    %s:%d\n      package %s          (layer: %s)\n        imports %s\n\n%s\n\n",
				headline, v.File, v.Line, v.Package, v.Layer, v.Imported, explain(v))
		}
	}
	fmt.Fprintf(&b, "%d violation(s) in %d packages.\n", len(r.Violations), r.Packages)
	return b.String()
}

// chainOf renders the hops between the reported package and the offending
// import, one indented line each. The first element is the package itself,
// already printed above it.
func chainOf(v Violation) string {
	var b strings.Builder
	for _, hop := range v.Via[1:] {
		fmt.Fprintf(&b, "        ↳ %s\n", hop)
	}
	return b.String()
}

func explain(v Violation) string {
	if (v.Rule == "transport" || v.Rule == "transport-chain") && v.Layer == "domain" {
		// The domain's version of the rule is the stronger one, and only one
		// of the two fixes below can apply to it.
		return "  The domain layer is the one part of the application that depends on\n" +
			"  nothing — not the other layers, and not a protocol. A domain type\n" +
			"  holding an HTTP client is reachable only where that client can be\n" +
			"  built, which means it cannot be tested, reused by a second\n" +
			"  transport, or moved into another service.\n\n" +
			"  Fix:\n" +
			"    • Declare a PORT in the domain — an interface saying what the\n" +
			"      domain needs, in the domain's own words — and put the client\n" +
			"      that speaks HTTP in infrastructure, where net/http is allowed.\n" +
			"      The domain then depends on the interface it owns."
	}
	if v.Rule == "transport-chain" {
		last := v.Via[len(v.Via)-1]
		return "  A handler knows nothing about how it is reached, and that holds\n" +
			"  through a helper as much as directly: this package does not import\n" +
			"  " + v.Imported + " itself, but everything it depends on comes with it.\n\n" +
			"  The import is in " + last + ".\n\n" +
			"  Fix one of:\n" +
			"    • SPLIT that package. The part the handler needs — a tenant, an\n" +
			"      identity, a decision — is almost always transport-free; only the\n" +
			"      edge middleware that reads a request needs " + v.Imported + ".\n" +
			"      Two packages, and the handler imports the half without it.\n" +
			"    • If the handler CALLS an external service, declare a port in the\n" +
			"      domain and put the client in infrastructure, where it is allowed."
	}
	if v.Rule == "transport" {
		return "  A handler knows nothing about how it is reached. It takes a request\n" +
			"  type and returns a response type; whether that arrived over HTTP,\n" +
			"  gRPC or a queue is the controller's business and the adapter's.\n\n" +
			"  That rule is what lets one Register call serve three protocols, and\n" +
			"  what makes the handler testable without a server.\n\n" +
			"  Fix one of:\n" +
			"    • Move the routing to the feature's controller.go, which is\n" +
			"      unlayered and is exactly where a use case meets a protocol.\n" +
			"    • If the handler CALLS an external service, declare a port in the\n" +
			"      domain and put the HTTP client in infrastructure, where net/http\n" +
			"      is allowed."
	}
	if v.Rule == "cross-module" {
		return "  This reaches into another feature module's internals. Modules talk\n" +
			"  through published events or an exported port, never by importing each\n" +
			"  other's packages — that is what makes extracting one into its own\n" +
			"  service a wiring change rather than a rewrite."
	}
	switch v.Layer {
	case "domain":
		return "  The domain layer imports nothing from the other three — that rule is\n" +
			"  why the domain can be tested, versioned, and extracted on its own.\n\n" +
			"  Fix one of:\n" +
			"    • Move the type you need into the domain, and let the other layer\n" +
			"      depend on it.\n" +
			"    • Declare a port in the domain and implement it in infrastructure,\n" +
			"      then wire the two in module.go — the one file permitted to see\n" +
			"      all four layers."
	case "application":
		return "  The application layer depends on the domain and on ports, never on a\n" +
			"  driver or a transport. Declare a port in the domain, implement it in\n" +
			"  infrastructure, and wire them in module.go."
	default:
		return "  Dependencies point inward: interfaces → application → domain ←\n" +
			"  infrastructure. Only module.go may see all four."
	}
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }
