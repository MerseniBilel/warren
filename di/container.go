package di

import (
	"reflect"
	"runtime"
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
	name     string // constructor as it prints: "postgres.NewUserRepository"
	site     string // constructor's file:line
	declared string // module declaration file:line, from DeclaredAt
	exported bool
	scope    *container
	inputs   []reflect.Type
	outputs  []reflect.Type
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
	if t.IsVariadic() {
		return errVariadic(funcName(v))
	}

	p := &provider{
		name:     funcName(v),
		site:     funcSite(v),
		declared: cfg.declared,
		exported: cfg.exported,
		scope:    c,
	}
	for in := range t.Ins() {
		p.inputs = append(p.inputs, in)
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

	if err := c.dig.Provide(constructor); err != nil {
		// Every anticipated failure is pre-checked above; whatever this is,
		// dig's wording must not leak (invariant 2).
		return errInternal(p.name)
	}
	c.providers = append(c.providers, p)
	return nil
}

func (c *container) Invoke(fn any) error {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return errNonConstructor(fn)
	}

	// Pre-check the whole subgraph this call needs, so any resolution failure
	// is Warren's diagnostic and anything left for dig to report is a
	// constructor's own error.
	for in := range v.Type().Ins() {
		if err := c.checkResolvable(in, nil, map[reflect.Type]bool{}); err != nil {
			return err
		}
	}

	if err := c.dig.Invoke(fn); err != nil {
		if cause := dig.RootCause(err); cause != nil && cause != err { //nolint:errorlint // dig.RootCause returns the exact constructor error; identity is the contract here
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

func (c *container) explain(t reflect.Type, seen map[reflect.Type]bool) Resolution {
	r := Resolution{Target: typeName(t)}
	visible := c.visibleProviders(t)
	if len(visible) == 0 || seen[t] {
		return r
	}
	seen[t] = true
	p := visible[0]
	r.Found = true
	r.Provider = p.name
	r.Scope = p.scope.name
	r.Site = p.site
	for _, in := range p.inputs {
		r.Inputs = append(r.Inputs, c.explain(in, seen))
	}
	return r
}

// checkResolvable walks the requirement closure of t from scope c, returning
// Warren's diagnostic for the first missing or ambiguous dependency.
func (c *container) checkResolvable(t reflect.Type, requirer *provider, seen map[reflect.Type]bool) error {
	if seen[t] {
		return nil
	}
	seen[t] = true
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

	var candidates []candidate
	for _, p := range c.root().subtreeProviders() {
		if p.provides(t) {
			candidates = append(candidates, candidate{provider: p.name, scope: p.scope.name, exported: p.exported})
		}
	}
	return errMissing(typeName(t), chain, declared, scope, candidates)
}

// consumerOf returns the first registered provider that consumes one of p's
// outputs, or nil — the next link of a requirement chain.
func (c *container) consumerOf(p *provider) *provider {
	for _, q := range c.subtreeProviders() {
		if q == p {
			continue
		}
		for _, in := range q.inputs {
			if p.provides(in) {
				return q
			}
		}
	}
	return nil
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
