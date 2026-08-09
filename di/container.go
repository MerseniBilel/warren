package di

import (
	stderrors "errors"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/dig"
)

// digScope is the slice of dig's API this package uses. *dig.Container and
// *dig.Scope both satisfy it; nothing else in the repository sees either type.
type digScope interface {
	Provide(constructor any, opts ...dig.ProvideOption) error
	Invoke(function any, opts ...dig.InvokeOption) error
	Scope(name string, opts ...dig.ScopeOption) *dig.Scope
}

// provider is this package's own record of one Provide call. Validation,
// Explain, and every diagnostic run off these records — dig is only asked to
// construct, never to explain.
type provider struct {
	name      string // constructor as it prints: "postgres.NewUserRepository"
	site      string // constructor's file:line
	declared  string // module declaration file:line, from DeclaredAt
	exported  bool
	forwarded string // origin scope when this is a re-export, else empty
	scope     *container
	inputs    []reflect.Type
	outputs   []reflect.Type
}

// primaryOutput is the type a provider is named by in a requirement chain.
func (p *provider) primaryOutput() string { return typeName(p.outputs[0]) }

func (p *provider) declarationSite() string {
	if p.declared != "" {
		return p.declared
	}
	return p.site
}

type container struct {
	dig       digScope
	name      string
	parent    *container
	children  map[string]*container
	providers []*provider
}

// New returns the root container.
//
// A container is not safe for concurrent use: Provide, Scope, Invoke, and
// Validate run from the single-threaded bootstrapper during boot steps 1–5,
// and the container is never consulted at request time (invariant 7). There
// is deliberately no lock to hide a concurrency bug behind.
func New() Container {
	return &container{dig: dig.New(), name: "root", children: map[string]*container{}}
}

func (c *container) Scope(name string) Container {
	if child, ok := c.children[name]; ok {
		return child
	}
	child := &container{dig: c.dig.Scope(name), name: name, parent: c, children: map[string]*container{}}
	c.children[name] = child
	return child
}

func (c *container) Provide(constructor any, opts ...ProvideOption) error {
	var cfg provideOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	v := reflect.ValueOf(constructor)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return errNonConstructor(constructor)
	}
	t := v.Type()

	name := cfg.name
	if name == "" {
		name = funcName(v)
	}
	p := &provider{
		name:      name,
		site:      funcSite(v),
		declared:  cfg.declared,
		exported:  cfg.exported,
		forwarded: cfg.forwardedFrom,
		scope:     c,
	}
	// A variadic constructor is provided with no variadic arguments — which
	// is exactly how warren's own option-taking constructors (memory.New,
	// inbox.NewMemoryStore, health.New) are meant to be used as providers.
	// Its trailing slice parameter is not a dependency to resolve.
	nIn := t.NumIn()
	if t.IsVariadic() {
		nIn--
	}
	for i := range nIn {
		p.inputs = append(p.inputs, t.In(i))
	}
	for out := range t.Outs() {
		if out != errType {
			p.outputs = append(p.outputs, out)
		}
	}
	if len(p.outputs) == 0 {
		return errNonConstructor(constructor)
	}

	// A same-scope duplicate would make dig fail with its own message at
	// Provide time; catch it here so the diagnostic is Warren's.
	for _, q := range c.providers {
		for _, out := range p.outputs {
			if q.provides(out) {
				return errAmbiguous(out, c.name, []*provider{q, p})
			}
		}
	}
	// A cycle would make dig fail with its own message at Provide time;
	// detect it first so the diagnostic is Warren's.
	if cycle := c.root().findCycle(p); cycle != nil {
		return errCycle(cycle)
	}

	if err := c.dig.Provide(attributed(v, p)); err != nil {
		// Every anticipated failure is pre-checked above; whatever this is,
		// dig's wording must not leak (invariant 2).
		return errInternal(p.name)
	}
	c.providers = append(c.providers, p)
	return nil
}

func (c *container) Invoke(fn any) (err error) {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return errNonConstructor(fn)
	}

	// A constructor may PANIC rather than return, and several of Warren's own
	// composition functions do so deliberately. dig calls the constructor, so
	// without this the panic leaves through dig's frames and invariant 2 is
	// broken by a stack trace instead of a message. Converting it to an error
	// also routes it through App.Run, which prints Warren's multi-line blocks
	// the way every other boot failure is printed.
	defer func() {
		if r := recover(); r != nil {
			err = errConstructorPanicked(c.name, r, string(debug.Stack()))
		}
	}()

	// Pre-check the whole subgraph this call needs, so any resolution failure
	// is Warren's diagnostic and anything left for dig to report is a
	// constructor's own error. The trailing variadic parameter, if any, is
	// skipped — dig invokes variadic functions without supplying it.
	t := v.Type()
	n := t.NumIn()
	if t.IsVariadic() {
		n--
	}
	for i := range n {
		if err := c.checkResolvable(t.In(i), nil, map[scopedType]bool{}); err != nil {
			return err
		}
	}

	if err := c.dig.Invoke(fn); err != nil {
		// Only a constructor's own error may surface. A dig-typed cause —
		// anything the pre-check failed to anticipate — is sanitized
		// (invariant 2: no dig wording reaches a caller). A cause that is
		// already a Warren diagnostic — a forwarding provider relaying a
		// failure from another scope — passes through unchanged, so one
		// failure renders one block however many import hops it crossed.
		var digErr dig.Error
		if cause := dig.RootCause(err); cause != nil && !stderrors.As(cause, &digErr) {
			var d *diagnostic
			if stderrors.As(cause, &d) {
				return d
			}
			return errConstructorFailed(c.name, cause)
		}
		return errInternal(funcName(v))
	}
	return nil
}

func (c *container) Validate() error {
	for _, p := range c.subtreeProviders() {
		for _, in := range p.inputs {
			visible := p.scope.visibleProviders(in)
			switch len(visible) {
			case 0:
				return c.missing(in, p)
			case 1:
			default:
				return errAmbiguous(in, p.scope.name, visible)
			}
		}
	}
	if cycle := c.root().findCycle(nil); cycle != nil {
		return errCycle(cycle)
	}
	return nil
}

func (c *container) Explain(target any) Resolution {
	return c.explain(targetType(target), map[reflect.Type]bool{})
}

// explain resolves t from scope c. path guards cycles only — it tracks the
// current recursion path, not everything visited, so a diamond (two inputs
// sharing a dependency) renders both occurrences as found.
func (c *container) explain(t reflect.Type, path map[reflect.Type]bool) Resolution {
	r := Resolution{Target: typeName(t)}
	visible := c.visibleProviders(t)
	if len(visible) == 0 {
		return r
	}
	p := visible[0]
	r.Found = true
	r.Provider = p.name
	r.Scope = p.scope.name
	r.Site = p.site
	if path[t] {
		return r
	}
	path[t] = true
	for _, in := range p.inputs {
		// The provider's own inputs resolve from ITS scope — the same rule
		// checkResolvable applies.
		r.Inputs = append(r.Inputs, p.scope.explain(in, path))
	}
	delete(path, t)
	return r
}

// scopedType keys resolvability checks: whether t resolves is relative to
// the asking scope, so the memo must be too — a type resolvable from a child
// scope may be missing from the root.
type scopedType struct {
	scope *container
	t     reflect.Type
}

// checkResolvable walks the requirement closure of t from scope c, returning
// Warren's diagnostic for the first missing or ambiguous dependency.
func (c *container) checkResolvable(t reflect.Type, requirer *provider, seen map[scopedType]bool) error {
	key := scopedType{scope: c, t: t}
	if seen[key] {
		return nil
	}
	seen[key] = true
	visible := c.visibleProviders(t)
	switch len(visible) {
	case 0:
		return c.missing(t, requirer)
	case 1:
	default:
		return errAmbiguous(t, c.name, visible)
	}
	p := visible[0]
	for _, in := range p.inputs {
		if err := p.scope.checkResolvable(in, p, seen); err != nil {
			return err
		}
	}
	return nil
}

// missing builds the golden missing-provider diagnostic: the requirement
// chain up to the module declaration, the scope verdict, and the candidates
// registered elsewhere.
func (c *container) missing(t reflect.Type, requirer *provider) error {
	scope := c.name
	var chain []string
	declared := ""
	if requirer != nil {
		scope = requirer.scope.name
		cur := requirer
		visited := map[*provider]bool{}
		for cur != nil && !visited[cur] {
			visited[cur] = true
			chain = append(chain, cur.primaryOutput())
			declared = cur.declarationSite()
			cur = c.root().consumerOf(cur)
		}
	}

	// A forwarder is a re-export, not a provider a user wrote: its scope holds
	// no constructor to export and its synthesized name is not valid Go. The
	// origin is in this same list, registered in the exporting scope, so
	// dropping forwarders loses nothing and stops the diagnostic suggesting a
	// fix that fails the boot (field-test defect B3).
	var candidates []candidate
	for _, p := range c.root().subtreeProviders() {
		if p.provides(t) && p.forwarded == "" {
			candidates = append(candidates, candidate{provider: p.name, scope: p.scope.name, exported: p.exported})
		}
	}
	// Nothing provides the type EXACTLY. Before giving up, look for a
	// provider whose output IMPLEMENTS it — that is the commonest wiring
	// mistake there is: a constructor whose return type is the concrete type
	// rather than the port. warren.Exports's own diagnostic already names
	// this mistake; this is the same help at the moment resolution fails.
	var implementers []candidate
	if len(candidates) == 0 && t.Kind() == reflect.Interface {
		for _, p := range c.root().subtreeProviders() {
			if p.forwarded != "" {
				continue
			}
			for _, out := range p.outputs {
				if out.Implements(t) {
					implementers = append(implementers, candidate{
						provider: p.name, scope: p.scope.name, concrete: typeName(out),
					})
					break
				}
			}
		}
	}
	return errMissing(typeName(t), chain, declared, scope, candidates, implementers)
}

// consumerOf returns a provider that consumes one of p's outputs AND can
// actually see p — the next link of a requirement chain. Same-scope
// consumers win; a type-name coincidence in an unrelated sibling scope must
// not route the chain (and its "declared in" line) to the wrong module.
func (c *container) consumerOf(p *provider) *provider {
	var fallback *provider
	for _, q := range c.subtreeProviders() {
		if q == p || !q.canSee(p) {
			continue
		}
		if !slices.ContainsFunc(q.inputs, p.provides) {
			continue
		}
		if q.scope == p.scope {
			return q
		}
		if fallback == nil {
			fallback = q
		}
	}
	return fallback
}

// canSee reports whether q's scope chain reaches p's scope — i.e. whether a
// resolution from q's scope could use p.
func (q *provider) canSee(p *provider) bool {
	for s := q.scope; s != nil; s = s.parent {
		if s == p.scope {
			return true
		}
	}
	return false
}

// findCycle looks for a provider cycle among the registered providers, plus
// extra when non-nil (the Provide-time pre-check). It returns the type names
// along the cycle, or nil.
func (c *container) findCycle(extra *provider) []string {
	providers := c.subtreeProviders()
	if extra != nil {
		providers = append(providers, extra)
	}
	const (
		unvisited = 0
		inStack   = 1
		done      = 2
	)
	state := map[*provider]int{}
	var stack []string
	var found []string

	var visit func(p *provider) bool
	visit = func(p *provider) bool {
		state[p] = inStack
		stack = append(stack, p.primaryOutput())
		for _, in := range p.inputs {
			for _, q := range visibleAmong(providers, p.scope, in) {
				switch state[q] {
				case inStack:
					start := 0
					target := q.primaryOutput()
					for i, name := range stack {
						if name == target {
							start = i
							break
						}
					}
					found = append(append([]string{}, stack[start:]...), target)
					return true
				case unvisited:
					if visit(q) {
						return true
					}
				}
			}
		}
		state[p] = done
		stack = stack[:len(stack)-1]
		return false
	}

	for _, p := range providers {
		if state[p] == unvisited && visit(p) {
			return found
		}
	}
	return nil
}

func (p *provider) provides(t reflect.Type) bool {
	return slices.Contains(p.outputs, t)
}

// visibleProviders returns the providers of t visible from this scope: the
// scope's own and its ancestors', in scope-then-ancestor order. Sibling
// scopes are invisible — that is the module boundary.
func (c *container) visibleProviders(t reflect.Type) []*provider {
	var out []*provider
	for s := c; s != nil; s = s.parent {
		for _, p := range s.providers {
			if p.provides(t) {
				out = append(out, p)
			}
		}
	}
	return out
}

// visibleAmong is visibleProviders over an explicit provider list — used by
// the Provide-time cycle pre-check, where one provider is not yet registered.
func visibleAmong(providers []*provider, from *container, t reflect.Type) []*provider {
	var out []*provider
	for _, p := range providers {
		if !p.provides(t) {
			continue
		}
		for s := from; s != nil; s = s.parent {
			if p.scope == s {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// subtreeProviders returns this scope's providers and every descendant's, in
// registration order within each scope, scopes in creation order.
func (c *container) subtreeProviders() []*provider {
	out := append([]*provider{}, c.providers...)
	for _, name := range c.childNames() {
		out = append(out, c.children[name].subtreeProviders()...)
	}
	return out
}

// childNames returns child scope names in creation order.
func (c *container) childNames() []string {
	names := make([]string, 0, len(c.children))
	for name := range c.children {
		names = append(names, name)
	}
	// Creation order is not tracked by the map; sort for determinism.
	slices.Sort(names)
	return names
}

func (c *container) root() *container {
	r := c
	for r.parent != nil {
		r = r.parent
	}
	return r
}

var errType = reflect.TypeFor[error]()

// typeName renders a type the way the diagnostics print it:
// "domain.UserRepository", "*user.RegisterUserHandler".
func typeName(t reflect.Type) string { return t.String() }

// targetType resolves Explain's argument: a value of the target type, or a
// nil pointer to it for interfaces.
func targetType(target any) reflect.Type {
	t := reflect.TypeOf(target)
	if t != nil && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Interface {
		return t.Elem()
	}
	return t
}

// funcName renders a function the way the diagnostics print it:
// "postgres.NewUserRepository".
func funcName(v reflect.Value) string {
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return "unknown function"
	}
	name := f.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// attributed returns a constructor of the SAME TYPE that tags any error the
// original returns with which provider it was.
//
// dig hands back only the root cause, so "✗ constructor failed / EOF" named
// neither a symbol nor a file:line — the one Warren diagnostic that did not,
// and a field test called it the worst of them. With a realistic opaque cause
// (EOF, "permission denied", a context deadline) the reader is left grepping
// a Providers list by hand.
//
// A constructor with no error return is handed back untouched, so the common
// case pays nothing. This is boot-time reflection over a function invoked
// once per scope; invariant 6 governs the REQUEST path, which never reaches
// here.
func attributed(v reflect.Value, p *provider) any {
	if !namesSomething(p) {
		return v.Interface()
	}
	t := v.Type()
	errIdx := -1
	for i := range t.NumOut() {
		if t.Out(i) == errType {
			errIdx = i
		}
	}
	if errIdx < 0 {
		return v.Interface()
	}
	call := v.Call
	if t.IsVariadic() {
		// MakeFunc delivers the variadic arguments already collected into the
		// final slice, which is CallSlice's calling convention, not Call's.
		call = v.CallSlice
	}
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		out := call(args)
		if e := out[errIdx]; !e.IsNil() {
			if err, ok := e.Interface().(error); ok && err != nil {
				out[errIdx] = reflect.ValueOf(&attributedError{
					name:  p.name,
					site:  p.site,
					cause: err,
				}).Convert(errType)
			}
		}
		return out
	}).Interface()
}

// namesSomething reports whether a provider's identity is worth printing.
//
// A closure — warren's own option-taking providers among them — prints as
// "http.Server.func1" and resolves to a site inside the runtime's assembly
// ("asm_arm64.s:29"), which is noise standing where a file:line belongs. The
// generic "A constructor" wording is better than a confident wrong answer, so
// the wrap is skipped entirely and the caller pays nothing.
func namesSomething(p *provider) bool {
	return !strings.Contains(p.name, ".func") && strings.Contains(p.site, ".go:")
}

// attributedError carries which constructor failed alongside its own error.
// It is unwrapped by errConstructorFailed and never reaches a user as itself,
// so its Error() is the cause verbatim — a stray print must not gain a prefix
// nobody asked for.
type attributedError struct {
	name  string
	site  string
	cause error
}

func (e *attributedError) Error() string { return e.cause.Error() }
func (e *attributedError) Unwrap() error { return e.cause }

func funcSite(v reflect.Value) string {
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return "unknown location"
	}
	file, line := f.FileLine(v.Pointer())
	if i := strings.LastIndex(file, "/"); i >= 0 {
		file = file[i+1:]
	}
	return file + ":" + strconv.Itoa(line)
}
