// Package warren owns application bootstrap, the module system, and the run
// loop.
//
// A module declaration is a value, not a side effect: NewModule returns an
// inert data structure and registers nothing. The bootstrapper walks the
// whole graph first, then materialises one DI scope per module, copies in
// only what each module's imports export, validates the whole graph, and
// only then instantiates anything — every error the framework can detect
// surfaces at boot, never on request 1.
package warren

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

// Module is an inert declaration of one module: its name, its imports, its
// providers, controllers, consumers, exports, and its lifecycle hooks.
// Constructing a Module registers nothing and performs no work.
//
// Because Imports carries Module values, an import cycle is unrepresentable:
// closing one would be infinite recursion in the user's own constructors
// before New is ever called. Cycles between providers are detected by
// warren/di.
type Module struct {
	name        string
	declared    string // file:line of the NewModule call
	id          *int   // identity: copies of one declaration share it; two NewModule calls never do
	imports     []Module
	providers   []any
	controllers []any
	consumers   []any
	exports     []reflect.Type
	onStart     []func(context.Context) error
	onStop      []func(context.Context) error
}

// ModuleOption configures a Module during NewModule.
type ModuleOption func(*Module)

// NewModule returns an inert Module value named name, configured by opts.
// Nothing is registered and no container is touched. The call site is
// recorded: it is the "declared in module.go:14" line of the missing-provider
// diagnostic.
func NewModule(name string, opts ...ModuleOption) Module {
	m := Module{name: name, declared: callerSite(), id: new(int)}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Imports declares the modules this module depends on. Only the imported
// modules' exported bindings become visible to it.
func Imports(modules ...Module) ModuleOption {
	return func(m *Module) { m.imports = append(m.imports, modules...) }
}

// Providers declares constructors owned by this module. A provider is
// private to its module unless its result type is also named in Exports.
//
// Constructors wire; OnStart acquires. A constructor that opens a connection
// or starts a goroutine owns a resource the boot sequence cannot release if
// a later module fails to build — put acquisition in an OnStart hook, whose
// rollback the lifecycle guarantees.
func Providers(constructors ...any) ModuleOption {
	return func(m *Module) { m.providers = append(m.providers, constructors...) }
}

// Controllers declares the constructors of this module's controllers. They
// are instantiated at boot; transport adapters register their routes at boot
// step 5.
func Controllers(controllers ...any) ModuleOption {
	return func(m *Module) { m.controllers = append(m.controllers, controllers...) }
}

// Consumers declares the constructors of this module's message consumers.
// They are instantiated at boot; broker adapters register them at boot
// step 5.
func Consumers(consumers ...any) ModuleOption {
	return func(m *Module) { m.consumers = append(m.consumers, consumers...) }
}

// Exports makes T resolvable by modules that import this module. Anything
// not exported stays private. T must be a declared return type of one of the
// module's providers, controllers, or consumers — exporting anything else is
// a boot error, and a constructor returning a concrete type does not match
// an exported interface: declare the constructor's return type as the
// interface.
func Exports[T any]() ModuleOption {
	t := reflect.TypeFor[T]()
	return func(m *Module) { m.exports = append(m.exports, t) }
}

// OnStart registers a startup hook for this module, run in dependency order
// at boot step 6.
func OnStart(fn func(context.Context) error) ModuleOption {
	return func(m *Module) { m.onStart = append(m.onStart, fn) }
}

// OnStop registers a shutdown hook for this module, run in reverse order at
// shutdown step 10.
func OnStop(fn func(context.Context) error) ModuleOption {
	return func(m *Module) { m.onStop = append(m.onStop, fn) }
}

// callerSite records where NewModule was called, trimmed to the last two
// path segments the way the diagnostics print sites.
func callerSite() string {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return "unknown location"
	}
	parts := strings.Split(file, "/")
	if len(parts) > 2 {
		file = strings.Join(parts[len(parts)-2:], "/")
	}
	return file + ":" + strconv.Itoa(line)
}

// outputsOf mirrors di's view of a constructor: its return types minus a
// trailing error. A non-function has no outputs; di produces the real
// diagnostic when it is provided.
func outputsOf(ctor any) []reflect.Type {
	v := reflect.ValueOf(ctor)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return nil
	}
	var out []reflect.Type
	for t := range v.Type().Outs() {
		if t != errType {
			out = append(out, t)
		}
	}
	return out
}

var errType = reflect.TypeFor[error]()

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errDuplicateModule(name, first, second string) error {
	if first == second {
		return diagnostic(fmt.Sprintf("✗ duplicate module name\n\n    %q is declared twice, both at %s — a module factory called twice\n    creates two distinct modules.\n\n  Declare the module once and share the value (a package-level variable), or\n  give each instance its own name.", name, first))
	}
	return diagnostic(fmt.Sprintf("✗ duplicate module name\n\n    %q is declared twice: %s and %s\n\n  Module names are scope names — every diagnostic addresses them — so they\n  must be unique.", name, first, second))
}

func errExportWithoutProvider(module, declared string, t reflect.Type) error {
	return diagnostic(fmt.Sprintf("✗ export without provider\n\n    module %q (%s) exports %s, but none of its providers returns it.\n\n  Add the constructor to warren.Providers, remove the export, or — if a\n  provider returns a concrete type that implements it — declare that\n  constructor's return type as the exported interface.", module, declared, t))
}
