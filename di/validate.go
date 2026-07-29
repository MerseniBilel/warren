package di

import (
	"reflect"
	"slices"

	"github.com/MerseniBilel/warren/errors"
)

// Validate reports every problem in the registered graph without constructing
// anything: a bad constructor signature, a duplicate, a missing provider, a
// cycle. Errors are joined, so one call reports all of them rather than the
// first, ordered by registration site.
//
// It is what warren doctor and warren lint call, and what [Container.Build] runs
// first.
func (c *Container) Validate() error {
	return join(c.validate(opValidate))
}

// join returns one problem as itself and several as a joined error.
//
// The single case is not an optimisation. errors.Join holds several unrelated
// errors, so errors.Detail reports no fields and no fix for one — and the fields
// are where the resolution chain, the requesting file, and the fix live. Since
// almost every failed boot has exactly one cause, returning it unwrapped is what
// keeps SPEC.md §6.1 renderable. A caller facing several renders each branch of
// the join separately.
func join(problems []error) error {
	if len(problems) == 1 {
		return problems[0]
	}

	return errors.Join(problems...)
}

// validate runs the three passes of SPEC.md §5.4, recording op as the entry
// point that failed so that Build's errors read "di.Build: …" rather than naming
// the internal walk.
func (c *Container) validate(op string) []error {
	problems := slices.Clone(c.regErrs)
	problems = append(problems, c.checkKeys()...)

	// Completeness and acyclicity are skipped when a registration was rejected:
	// that provider is not in the registry, so every type it would have provided
	// would be reported missing as well, and the real mistake would be buried
	// under its consequences.
	if len(problems) > 0 {
		return problems
	}

	problems = append(problems, c.checkCompleteness(op)...)

	return append(problems, c.checkAcyclic(op)...)
}

// checkKeys reports a type provided twice, and a type both provided and
// contributed. Both are walked in registration order so the output is stable.
func (c *Container) checkKeys() []error {
	var problems []error

	first := make(map[reflect.Type]*provider, len(c.order))
	conflicted := make(map[reflect.Type]bool)

	for _, p := range c.order {
		if p.kind == KindContributed {
			if provided, ok := c.single[p.typ]; ok && !conflicted[p.typ] {
				conflicted[p.typ] = true
				problems = append(problems, errProvidedAndContributed(provided, p))
			}

			continue
		}

		if earlier, ok := first[p.typ]; ok {
			problems = append(problems, errProvidedTwice(earlier, p))

			continue
		}

		first[p.typ] = p
	}

	return problems
}

// checkCompleteness walks every provider's dependencies and reports the first
// chain that reaches a type nobody provides.
func (c *Container) checkCompleteness(op string) []error {
	var (
		problems []error
		path     []reflect.Type
	)

	reported := make(map[reflect.Type]bool)
	visited := make(map[*provider]bool)

	var walk func(p *provider)

	walk = func(p *provider) {
		if visited[p] {
			return
		}

		visited[p] = true
		path = append(path, p.typ)

		defer func() { path = path[:len(path)-1] }()

		for _, dep := range p.deps {
			next := c.providersFor(dep)
			if len(next) > 0 {
				for _, n := range next {
					walk(n)
				}

				continue
			}

			if dep == contextType() || reported[dep] {
				continue
			}

			reported[dep] = true
			chain := append(slices.Clone(path), dep)
			problems = append(problems, errNoProvider(op, dep, chain, p.site, c.nearMiss(dep)))
		}
	}

	for _, p := range c.order {
		walk(p)
	}

	return problems
}

// checkAcyclic reports each dependency cycle once, as the path around it.
func (c *Container) checkAcyclic(op string) []error {
	const (
		unvisited = iota
		open
		closed
	)

	var (
		problems []error
		stack    []*provider
	)

	state := make(map[*provider]int, len(c.order))
	reported := make(map[reflect.Type]bool)

	var visit func(p *provider)

	visit = func(p *provider) {
		switch state[p] {
		case closed:
			return
		case open:
			if !reported[p.typ] {
				reported[p.typ] = true
				problems = append(problems, errCycle(op, cycleFrom(stack, p)))
			}

			return
		}

		state[p] = open
		stack = append(stack, p)

		for _, dep := range p.deps {
			for _, next := range c.providersFor(dep) {
				visit(next)
			}
		}

		stack = stack[:len(stack)-1]
		state[p] = closed
	}

	for _, p := range c.order {
		visit(p)
	}

	return problems
}

// cycleFrom returns the portion of the stack from p's first appearance onward,
// closed by p again so the path reads as a loop.
func cycleFrom(stack []*provider, p *provider) []*provider {
	start := slices.Index(stack, p)
	if start < 0 {
		return []*provider{p, p}
	}

	return append(slices.Clone(stack[start:]), p)
}

// providersFor returns the providers that satisfy a dependency: one for a
// provided type, every member for a group consumed as a slice, and none when it
// is unsatisfied.
//
// A directly provided []T wins over the group of T, so a constructor that wants
// the group is never surprised by one somebody registered whole.
func (c *Container) providersFor(dep reflect.Type) []*provider {
	if p, ok := c.single[dep]; ok {
		return []*provider{p}
	}

	if dep.Kind() == reflect.Slice {
		if members := c.grouped[dep.Elem()]; len(members) > 0 {
			return members
		}
	}

	return nil
}

// nearMiss returns a provider that is nearly what was asked for — the pointer
// when the value was wanted and the reverse, an implementation when an interface
// was wanted and the reverse. It is what turns SPEC.md §6.1 into §6.12.
func (c *Container) nearMiss(want reflect.Type) *provider {
	if want.Kind() == reflect.Pointer {
		if p, ok := c.single[want.Elem()]; ok {
			return p
		}
	} else if p, ok := c.single[reflect.PointerTo(want)]; ok {
		return p
	}

	for _, p := range c.order {
		if p.kind == KindContributed || p.typ == want {
			continue
		}

		if want.Kind() == reflect.Interface && p.typ.Implements(want) {
			return p
		}

		if p.typ.Kind() == reflect.Interface && want.Implements(p.typ) {
			return p
		}
	}

	return nil
}
