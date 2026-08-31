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

	// Features is the number of distinct feature modules found. Zero means
	// the cross-module rule compared nothing, which the report says out
	// loud: a check that did not run must never read as a check that
	// passed.
	Features int
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
	// The feature modules found, so the report can say whether the
	// cross-module rule compared anything at all.
	features := map[string]bool{}
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
		// Every early return below happens BEFORE the file is parsed, so each
		// one suppresses the rules for that file AND the import EDGES it
		// would have contributed to the graph — which means every chain
		// through it goes unreported too. Say both, always: the comment on
		// the module.go skip that used to live here was true about the
		// intent and silent about the effect, and that is how a black hole
		// swallowing every edge out of internal/platform survived review.
		// A rule that must exempt a file exempts it AFTER parsing, next to
		// the rule it belongs to.
		if d.IsDir() {
			// All four rules, and all edges, for everything beneath: test
			// fixtures are meant to break rules, and vendored code is not
			// the reader's to restructure.
			if path == dir {
				return nil // never skip the root, whatever it is called
			}
			name := d.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// All four rules, and all edges, for non-Go files and for tests. A
		// test importing its own infrastructure is how it is written.
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
			if f := featureOf(pkgPath); f != "" && !features[f] {
				features[f] = true
				report.Features++
			}
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			// A file that will not parse is skipped, not fatal: the check
			// exists to run on broken code. This suppresses all four rules
			// for the file and every edge out of it, so a chain through an
			// unparseable package is silently short. Nothing better is
			// available — there are no imports to read.
			return nil
		}

		layer := layerOf(pkgPath)
		feature := featureOf(pkgPath)

		// wiringFile is a feature's own module.go — internal/modules/<f>/ —
		// or the platform's. It is the file that wires a feature together,
		// and its ONE exemption is the cross-module rule below.
		//
		// The other three need no exemption and do not get one. The layer
		// rule fires only where layerOf is non-empty; the transport and
		// driver rules only in domain/ and application/. A wiring file at a
		// feature root or in internal/platform is unlayered, so all three
		// already pass over it by construction — the same argument
		// handlerLayers makes for controller.go. Honouring the NAME inside
		// application/ would hand every project a one-word opt-out of three
		// rules, which is why `layer == ""` is part of the test rather than
		// filepath.Base alone.
		//
		// The cross-module rule is different: featureOf IS non-empty at a
		// feature root, so without this the rule fires there — and the
		// linter's own remedy tells the reader to import "the owner's module
		// value in module.go". A tool that refuses the fix it prescribes is
		// worse than one with no remedy at all.
		wiringFile := filepath.Base(rel) == "module.go" && layer == ""

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

			// The transport and driver rules are the ones that look OUTSIDE
			// the project: what a handler must not import is the framework's
			// transport port, a driver, or net/http — none of which carry the
			// project's module path. Skipping every foreign import, as the
			// layer rules do, is why invariant 5 went unchecked, and why
			// README's "no pgx, no kgo" went unchecked after it.
			if opts.Rules&Layers != 0 && slices.Contains(handlerLayers, layer) {
				if rule := foreignRule(imported); rule != "" {
					report.Violations = append(report.Violations, Violation{
						File: rel, Line: pos.Line, Package: pkgPath, Layer: layer,
						Imported: imported, Rule: rule,
					})
					continue
				}
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
				if other := featureOf(imported); !wiringFile && feature != "" && other != "" && other != feature {
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
		findForeignChains(report, graph, site, modPath, "transport", isTransportPackage)
		findForeignChains(report, graph, site, modPath, "driver", isDriverPackage)
		findLaunderedImports(report, graph, site, modPath)
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

// findLaunderedImports reports a layer or cross-module violation committed
// THROUGH a helper package rather than directly.
//
// The direct rules read one file each, which makes them easy to satisfy by
// accident: move the import into a package outside internal/modules/ and the
// check goes quiet while the dependency is exactly as real. A field test
// proved both — an application layer reaching stock/infrastructure through
// internal/bridge, and one feature reaching another's domain through a type
// ALIAS in internal/shared, so the two modules share literally the same Go
// type. `go list -deps` saw both; `warren lint arch` said "No violations"
// and exited 0.
//
// Traversal goes through HELPERS ONLY — in-module packages that are neither
// layered nor inside a feature. That is not a shortcut, it is the rule: a
// layered intermediate breaks the rule at its own import and is reported
// there, and reporting it again at every package downstream turns one
// mistake into a wall of findings.
func findLaunderedImports(report *Report, graph map[string][]string, site map[string]Violation, modPath string) {
	// A package already reported directly needs no chain: the reader has the
	// import.
	direct := map[string]bool{}
	for _, v := range report.Violations {
		direct[v.Package+" "+v.Rule] = true
	}

	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	for _, pkg := range pkgs {
		layer, feature := layerOf(pkg), featureOf(pkg)
		if isHelper(pkg, modPath) {
			continue // a helper has no layer and no feature to violate
		}
		chain, reached := reachesThroughHelpers(graph, pkg, modPath, func(imported string) bool {
			if l := layerOf(imported); layer != "" && l != "" && slices.Contains(forbidden[layer], l) {
				return true
			}
			other := featureOf(imported)
			return feature != "" && other != "" && other != feature
		})
		if reached == "" {
			continue
		}
		rule := "cross-module-chain"
		if l := layerOf(reached); layer != "" && l != "" && slices.Contains(forbidden[layer], l) {
			rule = "layer-chain"
		}
		if direct[pkg+" "+strings.TrimSuffix(rule, "-chain")] {
			continue
		}
		v := site[pkg+" "+chain[1]]
		v.Rule = rule
		v.Imported = reached
		v.ImportedLayer = layerOf(reached)
		v.Via = chain
		report.Violations = append(report.Violations, v)
	}
}

// isHelper reports whether a package is one of the project's own and belongs
// to no layer and no feature — the shape a violation gets laundered through.
func isHelper(pkg, modPath string) bool {
	return strings.HasPrefix(pkg, modPath) && layerOf(pkg) == "" && featureOf(pkg) == ""
}

// reachesThroughHelpers returns the shortest chain from pkg to a package
// bad() accepts, passing only through helpers. Breadth-first, so the printed
// chain is the shortest one.
func reachesThroughHelpers(graph map[string][]string, pkg, modPath string, bad func(string) bool) ([]string, string) {
	type node struct {
		pkg  string
		path []string
	}
	seen := map[string]bool{pkg: true}
	// The first hop must be a helper: a direct bad import is the direct
	// rule's finding, not this one's.
	var queue []node
	for _, imp := range graph[pkg] {
		if isHelper(imp, modPath) && !seen[imp] {
			seen[imp] = true
			queue = append(queue, node{pkg: imp, path: []string{pkg, imp}})
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, imp := range graph[cur.pkg] {
			if bad(imp) {
				return cur.path, imp
			}
			if !isHelper(imp, modPath) || seen[imp] {
				continue
			}
			seen[imp] = true
			queue = append(queue, node{pkg: imp, path: append(append([]string{}, cur.path...), imp)})
		}
	}
	return nil, ""
}

// findForeignChains reports a handler-layer package that reaches an import
// the rule forbids — a transport package, or a driver — THROUGH another
// package in the same module. One function serves both rules: two
// breadth-first searches that differ by one predicate is how two rules drift
// apart.
//
// The direct rule reads one file and is easy to satisfy by accident: move
// the import into a helper and the check goes quiet. That is not a corner
// case — GETTING_STARTED tells you to write your own edge middleware with
// whttp.WriteError, and the obvious factoring puts it next to the tenant
// reader that the application layer needs, so a project following the
// documentation reaches net/http from its handlers and lints clean. A field
// test's application layer did, across 19 packages, and only `go list -deps`
// showed it. A pool handed round from an internal/store package is the same
// shape with a driver at the end of it.
//
// Only the project's OWN packages are traversed. A third-party helper that
// happens to import net/http is not something the reader can restructure,
// and a linter that reports it is one they switch off.
func findForeignChains(report *Report, graph map[string][]string, site map[string]Violation, modPath, rule string, offends func(string) bool) {
	// A package already reported directly under THIS rule is not reported
	// again: the reader has the import, and the chain to it adds nothing. The
	// other rule's finding is a different mistake and does not suppress it.
	direct := map[string]bool{}
	for _, v := range report.Violations {
		if v.Rule == rule {
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
		chain, offender := reachesForeign(graph, pkg, modPath, offends)
		if offender == "" {
			continue
		}
		// The line reported is the handler's own import of the next hop:
		// that is the edge the reader owns and the one they will change.
		v := site[pkg+" "+chain[1]]
		v.Rule = rule + "-chain"
		v.Imported = offender
		v.Via = chain
		report.Violations = append(report.Violations, v)
	}
}

// reachesForeign returns the shortest chain from pkg to an import offends()
// accepts, and that import. The chain starts at pkg and ends at the last
// in-module package before the offending import.
//
// Breadth-first, so the reported chain is the shortest one — a reader given
// a six-hop path when a two-hop one exists will not believe the tool.
func reachesForeign(graph map[string][]string, pkg, modPath string, offends func(string) bool) ([]string, string) {
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
			if offends(imp) {
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
	"github.com/valyala/fasthttp",
}

// isTransportPackage reports whether path is one of them, or lives beneath
// one — warren/transport/http is as much a transport package as its parent.
func isTransportPackage(path string) bool {
	return matchesPrefix(path, transportPackages)
}

// driverPackages are the import prefixes that make a package a driver. The
// list is explicit rather than heuristic, for the same reason
// transportPackages is: a linter that guesses is one people switch off.
//
// # Membership is decided by three tests, and all three must hold
//
// Apply them instead of re-arguing the list. Every entry below passes all
// three, and a candidate that fails any one of them does not go in — the
// remedy for it is the layer rule, a doc comment, or nothing.
//
//  1. SUBSTRATE, NOT SERVICE. A client for a data store or a message broker
//     the application ITSELF operates. This is what keeps the S3 SDK, Stripe
//     and Twilio out: those speak to a third party's product, not to storage
//     this service runs. A domain importing the S3 SDK is the same violation
//     in principle, and the rule that catches it is the layer rule, the
//     moment the port is declared in the domain and implemented in
//     infrastructure.
//
//  2. ENUMERABLE. The population is bounded and auditable. There are perhaps
//     thirty Go database and broker clients with meaningful use; there is no
//     bound at all on SaaS clients, so a list of them is permanently
//     incomplete and its incompleteness is indistinguishable from approval.
//     That is the real content of "an unbounded list is a heuristic in a
//     different hat" — the objection is to unboundedness, not to the
//     maintenance.
//
//  3. EVERY PATH IT HAS EVER HAD. A library on this list is on it under each
//     of its import paths: Shopify/sarama as well as IBM/sarama,
//     streadway/amqp as well as rabbitmq/amqp091-go, go-redis/redis as well
//     as redis/go-redis, jinzhu/gorm as well as gorm.io/gorm. Listing only
//     the current path enforces the rule against projects that have already
//     upgraded and exempts the ones that have not — which is exactly
//     backwards, because the codebase most likely to have a layering problem
//     is the one still on the old import.
//
// # Entries the reader will want the reasoning for
//
// database/sql IS here, and database/sql/driver with it. The
// legitimate-looking minority — a value object implementing driver.Valuer so
// it can be stored — is exactly the coupling Warren argues against.
//
// The ORMs and query builders — gorm, sqlx, bun, ent, xorm, squirrel,
// sqlboiler — follow from that entry rather than extending it. If a domain
// type implementing driver.Valuer "names its storage technology", a domain
// type carrying `gorm:"primaryKey"` tags names it harder, and an entity whose
// definition IS the schema names it hardest of all.
//
// warren/observability is here and stays here: warren.md §7.1 records it as a
// wiring module that wraps setup rather than an API, dragging 24 third-party
// modules behind it. Nothing in it is meant to be called from a use case. See
// driverKind — its remedy is not the repository one.
//
// warren/persistence and warren/broker are NOT here and must not be: they are
// contract packages, and a domain naming persistence.UnitOfWork is the
// pattern, not the violation. That is what the prefix matcher's `p+"/"` is
// for.
var driverPackages = []driverEntry{
	{"database/sql", store},
	// Postgres. pgx splits into pgconn/pgtype/pgproto3, each importable on
	// its own, so each is listed on its own.
	{"github.com/jackc/pgx", store},
	{"github.com/jackc/pgconn", store},
	{"github.com/jackc/pgtype", store},
	{"github.com/jackc/pgproto3", store},
	{"github.com/lib/pq", store},
	// MySQL, SQLite, SQL Server, ClickHouse.
	{"github.com/go-sql-driver/mysql", store},
	{"github.com/mattn/go-sqlite3", store},
	{"modernc.org/sqlite", store},
	{"github.com/microsoft/go-mssqldb", store},
	{"github.com/denisenkom/go-mssqldb", store},
	{"github.com/ClickHouse/clickhouse-go", store},
	// ORMs, query builders, and generated data layers.
	{"github.com/jinzhu/gorm", store},
	{"gorm.io/gorm", store},
	{"github.com/jmoiron/sqlx", store},
	{"github.com/uptrace/bun", store},
	{"entgo.io/ent", store},
	{"xorm.io/xorm", store},
	{"github.com/Masterminds/squirrel", store},
	{"github.com/volatiletech/sqlboiler", store},
	// Document, wide-column, search, and embedded stores.
	{"go.mongodb.org/mongo-driver", store},
	{"github.com/gocql/gocql", store},
	{"github.com/elastic/go-elasticsearch", store},
	{"github.com/opensearch-project/opensearch-go", store},
	{"github.com/couchbase/gocb", store},
	{"go.etcd.io/bbolt", store},
	{"github.com/dgraph-io/badger", store},
	// Key-value and cache.
	{"github.com/redis/go-redis", store},
	{"github.com/go-redis/redis", store},
	// Brokers.
	{"github.com/twmb/franz-go", broker},
	{"github.com/segmentio/kafka-go", broker},
	{"github.com/IBM/sarama", broker},
	{"github.com/Shopify/sarama", broker},
	{"github.com/rabbitmq/amqp091-go", broker},
	{"github.com/streadway/amqp", broker},
	{"github.com/nats-io/nats.go", broker},
	{"github.com/apache/pulsar-client-go", broker},
	// Warren's own adapter modules.
	{"github.com/MerseniBilel/warren/persistence/postgres", store},
	{"github.com/MerseniBilel/warren/persistence/mysql", store},
	{"github.com/MerseniBilel/warren/persistence/mongo", store},
	{"github.com/MerseniBilel/warren/persistence/redis", store},
	{"github.com/MerseniBilel/warren/broker/kafka", broker},
	{"github.com/MerseniBilel/warren/broker/rabbitmq", broker},
	{"github.com/MerseniBilel/warren/broker/nats", broker},
	{"github.com/MerseniBilel/warren/broker/memory", broker},
	{"github.com/MerseniBilel/warren/observability", wiring},
}

// driverEntry is one listed prefix and the KIND of driver it is. The kind
// rides on the entry rather than in a second list beside it, because two
// lists that must agree are two lists that will not: a driver added to one
// and forgotten in the other would be reported with a remedy written for
// something else, which is defect 4 in a new shape.
type driverEntry struct {
	path string
	kind string
}

// The three kinds of driver, which are three different mistakes with three
// different fixes. "Declare a port and let `warren g repository` write both
// halves" is sound advice for pgx, useless for a Kafka client, and nonsense
// for a module that only installs exporters at boot.
const (
	// store — a data store: the handler has a port to declare and a
	// repository to generate.
	store = "store"
	// broker — a message broker: the handler has a port to TAKE
	// (broker.Publisher), or an event to consume.
	broker = "broker"
	// wiring — a module that is composed at boot and never called from a use
	// case. There is nothing to declare; the import simply should not be
	// there.
	wiring = "wiring"
)

// isDriverPackage reports whether path is one of them, or lives beneath one —
// pgx/v5/pgxpool is as much a driver as pgx itself.
func isDriverPackage(path string) bool {
	return driverKind(path) != ""
}

// driverKind classifies a driver import as store, broker or wiring, and
// returns "" for an import that is not a driver at all. The boundary is a
// slash, never a string prefix — see matchesPrefix.
func driverKind(path string) string {
	for _, e := range driverPackages {
		if path == e.path || strings.HasPrefix(path, e.path+"/") {
			return e.kind
		}
	}
	return ""
}

// matchesPrefix reports whether path is one of the listed packages or lives
// beneath one. The boundary is a slash, never a string prefix: listing
// warren/persistence/postgres must never catch warren/persistence, which is
// the port every handler is supposed to name.
func matchesPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// foreignRule names the outside-the-project rule an import breaks, or "" for
// an import that breaks neither. The two are separate rules because they have
// separate remedies: "move the routing to the controller" is nonsense advice
// for a pgx import.
func foreignRule(path string) string {
	switch {
	case isTransportPackage(path):
		return "transport"
	case isDriverPackage(path):
		return "driver"
	}
	return ""
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
		return fmt.Sprintf("No violations in %d packages.\n\n"+
			"  Checked: the layer rule, the handler/transport rule and the handler/driver\n"+
			"  rule — each directly and through a helper package.\n\n%s\n%s",
			r.Packages, r.crossModuleCoverage(), coverageNotes())
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
		case "layer-chain":
			fmt.Fprintf(&b, "✗ layer violation, through a helper\n\n    %s:%d\n      package %s          (layer: %s)\n%s        imports %s (layer: %s)\n\n%s\n\n",
				v.File, v.Line, v.Package, v.Layer, chainOf(v), v.Imported, v.ImportedLayer, explain(v))
		case "cross-module-chain":
			fmt.Fprintf(&b, "✗ cross-module import, through a helper\n\n    %s:%d\n      package %s\n%s        imports %s\n\n%s\n\n",
				v.File, v.Line, v.Package, chainOf(v), v.Imported, explain(v))
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
		case "driver-chain":
			what := "handler"
			if v.Layer == "domain" {
				what = "domain"
			}
			fmt.Fprintf(&b, "✗ the %s reaches a driver\n\n    %s:%d\n      package %s          (layer: %s)\n%s        imports %s\n\n%s\n\n",
				what, v.File, v.Line, v.Package, v.Layer, chainOf(v), v.Imported, explain(v))
		case "driver":
			// The same split as the transport rule, and for the same reason:
			// a use case has a port to declare, and a value object being told
			// it has a handler is how a reader decides the tool has not read
			// their code.
			headline := "handler imports a driver"
			if v.Layer == "domain" {
				headline = "the domain imports a driver"
			}
			fmt.Fprintf(&b, "✗ %s\n\n    %s:%d\n      package %s          (layer: %s)\n        imports %s\n\n%s\n\n",
				headline, v.File, v.Line, v.Package, v.Layer, v.Imported, explain(v))
		}
	}
	fmt.Fprintf(&b, "%d violation(s) in %d packages.\n\n%s", len(r.Violations), r.Packages, r.crossModuleCoverage())
	return b.String()
}

// crossModuleCoverage says whether the cross-module rule compared anything.
//
// A check that did not run must never read as a check that passed: under the
// layout with no `modules` segment, featureOf returns "" for every package,
// the cross-module rule and its through-a-helper variant are both skipped,
// and a real cross-feature import used to print "No violations" and exit 0.
// The tool does not GUESS where a feature begins — a heuristic fires on the
// project root of a single-feature app and invents the boundary it then
// polices — so it says what it did instead.
func (r *Report) crossModuleCoverage() string {
	if r.Features > 0 {
		unit := "feature modules"
		if r.Features == 1 {
			unit = "feature module"
		}
		return fmt.Sprintf("  Checked %d %s for cross-module imports.\n", r.Features, unit)
	}
	return "  NOT checked: the cross-module rule. It compares feature modules, which it\n" +
		"  finds by a `modules` path segment — internal/modules/<feature>/… , the tree\n" +
		"  `warren new` generates — and this project has none, so nothing was compared\n" +
		"  across features.\n"
}

// coverageNotes states what the walk deliberately does NOT look at.
//
// Both facts below change what a clean report MEANS, and neither was stated
// until 2026-08-29 — an external reviewer found the first by planting a
// two-rule violation in a _test.go file and watching the report say "No
// violations", and the second by splitting module.go in two and getting a
// violation they could not explain. A report that discloses its own blind
// spots is the thing that makes this tool trustworthy; a silent exclusion is
// the thing that unmakes it.
func coverageNotes() string {
	return "  NOT checked: _test.go files. A test importing its own feature's\n" +
		"  infrastructure is how a test is written, so the walk skips them — which\n" +
		"  also means a test file is the one place these rules do not reach.\n\n" +
		"  Carve-out: a feature's module.go MAY import a sibling feature. That is\n" +
		"  where a module value is wired, so the cross-module rule exempts that one\n" +
		"  filename and no other.\n"
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

// brokerRemedy is the fix list for a handler that named a broker client. It is
// shared by the direct rule and the chain, indented to suit each, because the
// advice is identical and two copies of it would drift.
func brokerRemedy(indent string) string {
	return indent + "• To PUBLISH, take broker.Publisher. It is a CONTRACT package, so a\n" +
		indent + "  handler may name it — that is the pattern, not the violation — and\n" +
		indent + "  main.go decides which technology is behind it.\n" +
		indent + "• To REACT to something that happened, consume the EVENT rather than\n" +
		indent + "  driving a client here. `warren g consumer` writes the handler and the\n" +
		indent + "  subscription; the handler it writes names no broker at all."
}

// wiringRemedy is the fix list for a handler that named a boot-wiring module.
// There is no port to declare: the answer is that the handler already has what
// it was reaching for.
func wiringRemedy(indent string) string {
	return indent + "• A handler that needs a SPAN takes the tracer from OpenTelemetry's own\n" +
		indent + "  global provider, which this module has already configured at boot.\n" +
		indent + "  No Warren import is involved.\n" +
		indent + "• Correlation fields — trace_id, span_id, correlation_id — are already\n" +
		indent + "  on log.FromContext(ctx). Log through it and they are there, with no\n" +
		indent + "  import at all.\n" +
		indent + "• Everything else the module exposes is setup, and setup belongs in\n" +
		indent + "  main.go."
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
	if (v.Rule == "driver" || v.Rule == "driver-chain") && v.Layer == "domain" {
		// The domain's version of the rule is the stronger one, and the
		// driver.Valuer case is named because it is the one a reader will
		// otherwise defend: their Money type is being reported and they
		// cannot see why.
		return "  The domain layer is the one part of the application that depends on\n" +
			"  nothing — not the other layers, not a protocol, and not a database. A\n" +
			"  domain type that knows how it is stored cannot be tested without the\n" +
			"  store, versioned apart from it, or moved into another service.\n\n" +
			"  This includes implementing driver.Valuer or sql.Scanner on a domain type.\n" +
			"  It reads as a small convenience and it is the whole coupling: the type now\n" +
			"  names its storage technology.\n\n" +
			"  Fix:\n" +
			"    • Declare a PORT in the domain and put the mapping in infrastructure,\n" +
			"      where database/sql is allowed. A repository that translates between the\n" +
			"      domain type and its columns is the pattern; the domain type stays plain."
	}
	if v.Rule == "driver-chain" {
		last := v.Via[len(v.Via)-1]
		lead := "  A use case says WHAT must happen, never HOW it is stored or delivered, and\n" +
			"  that holds through a helper as much as directly: this package does not\n" +
			"  import " + v.Imported + " itself, but everything it depends on comes\n" +
			"  with it.\n\n" +
			"  The import is in " + last + ".\n\n"
		switch driverKind(v.Imported) {
		case wiring:
			return "  This is a wiring module, not an API:\n\n" +
				"      " + v.Imported + "\n\n" +
				"  installs the exporters and composes app.Traced and app.Metered around\n" +
				"  every route at BOOT (warren.md §7.1). This package does not import it\n" +
				"  itself, and everything it depends on comes with it all the same.\n\n" +
				"  The import is in " + last + ".\n\n" +
				"  Fix one of:\n" +
				"    • SPLIT that package, and keep the boot wiring out of the half a\n" +
				"      handler imports. Setup belongs in main.go.\n" +
				wiringRemedy("    ")
		case broker:
			return lead + "  Fix one of:\n" +
				"    • SPLIT that package. The part the handler needs — an identifier, a\n" +
				"      decision, a plain value — is almost always broker-free; only the\n" +
				"      code that publishes or subscribes needs " + v.Imported + ".\n" +
				"      Two packages, and the handler imports the half without it.\n" +
				brokerRemedy("    ")
		default:
			return lead + "  Fix one of:\n" +
				"    • SPLIT that package. The part the handler needs — an identifier, a\n" +
				"      decision, a plain value — is almost always driver-free; only the code\n" +
				"      that talks to the store needs " + v.Imported + ".\n" +
				"      Two packages, and the handler imports the half without it.\n" +
				"    • If the handler needs the STORE, declare a port in the domain and put\n" +
				"      the " + v.Imported + " code in infrastructure, where a driver is\n" +
				"      allowed. `warren g repository` writes both halves."
		}
	}
	if v.Rule == "driver" {
		switch driverKind(v.Imported) {
		case wiring:
			return "  This is a wiring module, not an API:\n\n" +
				"      " + v.Imported + "\n\n" +
				"  installs the exporters and composes app.Traced and app.Metered around\n" +
				"  every route at BOOT (warren.md §7.1). A handler that imports it changes\n" +
				"  nothing about what it is instrumented with, and pays for the module's\n" +
				"  transitive dependencies in every build that names it.\n\n" +
				"  Fix one of:\n" + wiringRemedy("    ")
		case broker:
			return "  A use case says WHAT must happen, never HOW it is delivered. The moment it\n" +
				"  names " + v.Imported + " it can only run where that broker can be\n" +
				"  reached: not in a unit test, not behind a second driver, not in another\n" +
				"  service.\n\n" +
				"  That rule is what makes app.Handler[Req, Res] the same type on HTTP, gRPC\n" +
				"  and a consumer, and what makes swapping Kafka for RabbitMQ one line of\n" +
				"  main.go.\n\n" +
				"  Fix one of:\n" + brokerRemedy("    ")
		default:
			return "  A use case says WHAT must happen, never HOW it is stored. The moment it\n" +
				"  names " + v.Imported + " it can only run where that store can be\n" +
				"  reached: not in a unit test, not behind a second driver, not in another\n" +
				"  service.\n\n" +
				"  That rule is what makes app.Handler[Req, Res] the same type on HTTP, gRPC\n" +
				"  and a consumer, and what lets warren/persistence/postgres be swapped for\n" +
				"  another driver in one line of main.go.\n\n" +
				"  Fix:\n" +
				"    • Declare a PORT in the domain — an interface in the domain's own words —\n" +
				"      and put the " + v.Imported + " code in infrastructure, where a driver\n" +
				"      is allowed. Wire the two in module.go — the feature's own wiring file,\n" +
				"      and the only one permitted to see all four layers.\n" +
				"      `warren g repository` writes both halves."
		}
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
	if v.Rule == "layer-chain" || v.Rule == "cross-module-chain" {
		last := v.Via[len(v.Via)-1]
		what := "another feature module's internals"
		if v.Rule == "layer-chain" {
			what = "the " + v.ImportedLayer + " layer"
		}
		return "  This package does not import " + v.Imported + " itself, and it depends\n" +
			"  on it all the same: everything a package imports comes with it. The\n" +
			"  dependency is as real as a direct one — `go list -deps` shows it —\n" +
			"  and only the LINE is somewhere else.\n\n" +
			"  The import is in " + last + ", which belongs to no layer and no\n" +
			"  feature, so nothing else objected on the way through.\n\n" +
			"  Fix one of:\n" +
			"    • Move what this package actually needs out of " + last + "\n" +
			"      and into somewhere it may legally live — usually a port in the\n" +
			"      domain, implemented in infrastructure and wired in module.go.\n" +
			"    • Split " + last + ", so the half this imports no longer\n" +
			"      reaches " + what + ".\n\n" +
			"  A type ALIAS counts: `type Item = other.Item` makes it the same Go\n" +
			"  type, which is total coupling with a local name."
	}
	if v.Rule == "cross-module" {
		// The remedy used to end at "or an exported port" — a route this rule
		// makes structurally unreachable, because naming another feature's
		// interface type requires importing the package that declares it. The
		// DI diagnostic recommends warren.Exports for exactly this case, so
		// the two tools instructed users in a circle. Architect ruling,
		// 2026-08-08: the sanctioned home for a shared port is a
		// self-contained package OUTSIDE internal/modules/, which featureOf
		// already exempts and findLaunderedImports already polices.
		return "  This reaches into another feature module's internals. Modules talk\n" +
			"  through published events or an exported port, never by importing each\n" +
			"  other's packages — that is what makes extracting one into its own\n" +
			"  service a wiring change rather than a rewrite.\n\n" +
			"  Fix one of:\n" +
			"    • If this feature is REACTING to something that happened in the\n" +
			"      other, consume its EVENT. `warren g consumer` writes the handler\n" +
			"      and the subscription. The event is a wire contract, so the other\n" +
			"      feature stays extractable. This is the preferred answer.\n" +
			"    • If this feature needs to ASK the other something, the port has to\n" +
			"      live where both may see it — which is NOT inside either feature.\n" +
			"      Move the interface to a self-contained package outside\n" +
			"      internal/modules/:\n\n" +
			"          internal/contracts/<owner>/  — the interface and its own\n" +
			"          request and result types, importing no feature package.\n\n" +
			"      The owner's infrastructure implements it, the owner's module\n" +
			"      exports it with warren.Exports[...](), and this feature imports\n" +
			"      the contract package and the owner's module value in module.go.\n" +
			"      A contract package that imports a feature is reported here too,\n" +
			"      as a cross-module import through a helper."
	}
	switch v.Layer {
	case "domain":
		return "  The domain layer imports nothing from the other three — that rule is\n" +
			"  why the domain can be tested, versioned, and extracted on its own.\n\n" +
			"  Fix one of:\n" +
			"    • Move the type you need into the domain, and let the other layer\n" +
			"      depend on it.\n" +
			"    • Declare a port in the domain and implement it in infrastructure,\n" +
			"      then wire the two in the feature's own module.go — the one file\n" +
			"      permitted to see all four layers."
	case "application":
		return "  The application layer depends on the domain and on ports, never on a\n" +
			"  driver or a transport. Declare a port in the domain, implement it in\n" +
			"  infrastructure, and wire them in module.go."
	default:
		return "  Dependencies point inward: interfaces → application → domain ←\n" +
			"  infrastructure. Only the feature's own module.go may see all four."
	}
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }
