// Package di owns Warren's dependency-injection container: scoping, graph
// validation, and diagnostics.
//
// The root container holds process-wide bindings; each module gets a child
// scope, and a binding registered in a scope is private to that scope unless
// the module exported it. The whole graph is validated before anything is
// instantiated — a missing provider is a startup failure with a full
// resolution chain and a copy-pasteable fix, never a nil-pointer panic in
// production.
//
// go.uber.org/dig is imported by this package and by nothing else in the
// repository. No dig type and no dig error text ever reaches a caller.
package di

import (
	"fmt"
	"strings"
)

// Container is a dependency-injection scope. The root container holds
// process-wide bindings (config, logger, tracer); each module gets a child
// container via Scope, and a child sees only the bindings its imports export.
type Container interface {
	// Provide registers a constructor in this scope. A constructor is a
	// function whose returns are the types it provides, optionally with a
	// trailing error.
	Provide(constructor any, opts ...ProvideOption) error

	// Invoke resolves fn's parameters from this scope and calls it.
	Invoke(fn any) error

	// Scope returns the child container named name, creating it on first use:
	// repeat calls with the same name return the same child. The child is the
	// module boundary: bindings registered in it are private to it unless
	// exported.
	Scope(name string) Container

	// Validate checks the whole graph reachable from this scope: is every
	// dependency resolvable, and is anything ambiguous. It is boot step 3 and
	// runs before any singleton is instantiated — it constructs nothing.
	Validate() error

	// Explain reports how target would be resolved from this scope. It powers
	// `warren explain di`. Pass a value of the target type, or a nil pointer
	// to it for interfaces: Explain((*domain.UserRepository)(nil)).
	Explain(target any) Resolution
}

// ProvideOption configures a single Provide call. It is Warren's own type: no
// dig option type appears in any Warren signature.
type ProvideOption func(*provideOptions)

type provideOptions struct {
	exported bool
	declared string
	name     string
}

// Exported marks the binding as exported from its module: visible to the
// modules that import this one. The bootstrapper reads this when it copies
// exported bindings into importing scopes, and the missing-provider
// diagnostic reads it to tell "not exported" apart from "not provided".
func Exported() ProvideOption {
	return func(o *provideOptions) { o.exported = true }
}

// DeclaredAt records where the module declaration that owns this binding
// lives, as a file and line. The root package captures it at NewModule time;
// it is the "declared in module.go:14" line of the missing-provider
// diagnostic. Without it, diagnostics fall back to the constructor's own
// position.
func DeclaredAt(file string, line int) ProvideOption {
	return func(o *provideOptions) { o.declared = fmt.Sprintf("%s:%d", file, line) }
}

// Named overrides the constructor name diagnostics print. Synthesized
// constructors — reflect.MakeFunc values, whose runtime name is an assembly
// stub — must pass it, so an ambiguous-binding or candidate line names the
// real actor ("*pool (exported by module \"platform\")"), never
// reflect.makeFuncStub.
func Named(name string) ProvideOption {
	return func(o *provideOptions) { o.name = name }
}

// Resolution is the result of Explain: how one target resolves from one
// scope, and, recursively, how each of its constructor's parameters resolves.
type Resolution struct {
	// Target is the type asked about, as it prints: "domain.UserRepository".
	Target string

	// Found reports whether a provider is visible from the asking scope.
	Found bool

	// Provider is the constructor that provides Target, empty when not Found.
	Provider string

	// Scope is the scope the provider is registered in, empty when not Found.
	Scope string

	// Site is the constructor's file:line, empty when not Found.
	Site string

	// Inputs are the resolutions of the provider's own parameters.
	Inputs []Resolution
}

// String renders the resolution as an indented tree, one line per step —
// the output of `warren explain di`.
func (r Resolution) String() string {
	var b strings.Builder
	r.render(&b, 0)
	return strings.TrimRight(b.String(), "\n")
}

func (r Resolution) render(b *strings.Builder, depth int) {
	pad := strings.Repeat(" ", depth*5)
	b.WriteString(pad + r.Target + "\n")
	if !r.Found {
		b.WriteString(pad + "  └─ no provider visible from this scope\n")
		return
	}
	fmt.Fprintf(b, "%s  └─ provided by %s (scope %q, %s)\n", pad, r.Provider, r.Scope, r.Site)
	for _, in := range r.Inputs {
		in.render(b, depth+1)
	}
}

// Resolve resolves T from c.
func Resolve[T any](c Container) (T, error) {
	var out T
	err := c.Invoke(func(t T) { out = t })
	return out, err
}

// MustResolve resolves T from c and panics with the resolution diagnostic if
// it cannot. It is the kernel's one sanctioned panic (AGENT.md § General
// names the exception): it runs at boot, where the alternative to failing
// loudly is handing back a zero value that explodes later. Application boot
// code may prefer it; library code uses Resolve.
func MustResolve[T any](c Container) T {
	t, err := Resolve[T](c)
	if err != nil {
		panic(err)
	}
	return t
}
