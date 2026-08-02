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

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") && name != "." {
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
			if uerr != nil || !strings.HasPrefix(imported, modPath) {
				continue // third party and stdlib are not this rule's business
			}
			pos := fset.Position(spec.Pos())

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

	sort.Slice(report.Violations, func(i, j int) bool {
		a, b := report.Violations[i], report.Violations[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return report, nil
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
		}
	}
	fmt.Fprintf(&b, "%d violation(s) in %d packages.\n", len(r.Violations), r.Packages)
	return b.String()
}

func explain(v Violation) string {
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
