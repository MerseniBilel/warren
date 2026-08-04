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
	"slices"
	"strconv"
	"strings"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/validate"
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
	eager       []reflect.Type
	controllers []any
	consumers   []any
	exports     []reflect.Type
	optional    []reflect.Type
	onStart     []func(context.Context) error
	onStop      []func(context.Context) error
}

// ModuleOption configures a Module during NewModule.
type ModuleOption func(*Module)

// NewModule returns an inert Module value named name, configured by opts.
// Nothing is registered and no container is touched. The call site is
// recorded: it is the "declared in module.go:14" line of the missing-provider
// diagnostic.
//
// Declare each module ONCE. Modules are deduplicated by identity, so the
// natural `func Module() warren.Module` factory produces two distinct
// modules the moment two features import it, and two modules sharing a name
// is a boot error. The idiom that reads like a function and yields one
// identity:
//
//	var Module = sync.OnceValue(func() warren.Module {
//	    return warren.NewModule("platform", ...)
//	})
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
// step 5. A controller's constructor is also a provider — list it here only,
// not in Providers too, or the duplicate registers as an ambiguous binding.
func Controllers(controllers ...any) ModuleOption {
	return func(m *Module) { m.controllers = append(m.controllers, controllers...) }
}

// Consumers declares the constructors of this module's message consumers.
// They are instantiated at boot; broker adapters register them at boot
// step 5. Like controllers, a consumer's constructor is also a provider —
// list it in one place only.
func Consumers(consumers ...any) ModuleOption {
	return func(m *Module) { m.consumers = append(m.consumers, consumers...) }
}

// Eager declares that T is materialised at boot even when nothing in the
// graph consumes it — for modules whose provider's construction IS the
// point. config.Module uses it so a bad config fails the boot even if no
// constructor injects the struct; without it, an unconsumed provider is
// simply never built.
func Eager[T any]() ModuleOption {
	t := reflect.TypeFor[T]()
	return func(m *Module) { m.eager = append(m.eager, t) }
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

// Optional declares that a nil T from one of this module's providers is
// MEANT, and must not fail the boot the way an undeclared nil does.
//
// A provider returning nil is normally a boot error: warren.md §1.3's rule is
// that every detectable error surfaces at boot, and a nil interface otherwise
// booted clean and became a 500 on the first request to touch it. But some
// capabilities are legitimately absent. warren/observability returns a nil
// app.Telemetry when no collector is configured, and app.WithTelemetry drops
// a nil so the uninstrumented request path stays a pass-through — a no-op
// value instead would ride every request context and cost real work per
// request, which is the property the nil exists to preserve.
//
// Optional is per TYPE, not per module: declaring one absence does not disarm
// the check for anything else the module provides. Consumers of an optional
// binding must handle the nil — that is the contract they are opting into.
func Optional[T any]() ModuleOption {
	t := reflect.TypeFor[T]()
	return func(m *Module) { m.optional = append(m.optional, t) }
}

// OnStart registers a startup hook for this module, run in dependency order
// at boot step 6. The hook is a plain closure fixed at declaration time and
// resolves nothing from the container; a hook that needs something built at
// boot — a consumer pipeline's drain func, a connection opened by a
// constructor — is registered the other way: the constructor injects
// lifecycle.Lifecycle (provided in the root scope) and appends its own
// lifecycle.Hook.
func OnStart(fn func(context.Context) error) ModuleOption {
	return func(m *Module) { m.onStart = append(m.onStart, fn) }
}

// OnStop registers a shutdown hook for this module, run in reverse order at
// shutdown step 10. See OnStart for the boot-time-created alternative — the
// injected-Lifecycle pattern is how a consumer registers its drain.
func OnStop(fn func(context.Context) error) ModuleOption {
	return func(m *Module) { m.onStop = append(m.onStop, fn) }
}

// Name reports the module's name — the scope App.Invoke addresses and the
// name diagnostics print.
func (m Module) Name() string { return m.name }

// Substitution replaces or adds a binding before boot. It is the seam test
// harnesses use to inject fakes, and main can use it to provide a value it
// computed itself.
type Substitution struct {
	t       reflect.Type
	value   any
	replace bool
	site    string
}

// Substitute replaces every provider of T with v. An unmatched substitution
// is a boot error naming T — a typo'd fake is never silently ignored, which
// is the failure mode that makes test doubles untrustworthy.
func Substitute[T any](v T) Substitution {
	return Substitution{t: reflect.TypeFor[T](), value: v, replace: true, site: callerSite()}
}

// Bind provides v as T in the root scope, where every module can see it.
//
// If the graph already provides T, Bind REPLACES that provider rather than
// colliding with it: a harness binding a fake broker into an application
// whose platform module provides a real one is the normal case, and an
// ambiguous-binding failure there would be useless. Use Substitute when the
// replacement is required — it fails the boot if nothing matched.
func Bind[T any](v T) Substitution {
	return Substitution{t: reflect.TypeFor[T](), value: v, site: callerSite()}
}

// callerSite records where the module was really declared, trimmed to the
// last two path segments the way the diagnostics print sites. Frames inside
// the framework itself are skipped, so a module FACTORY — config.Module,
// or any future warren-provided one — records its caller's line, not its
// own body: a diagnostic pointing into the framework's files can never help
// the user find their call site.
func callerSite() string {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function != "" &&
			!strings.HasPrefix(f.Function, "github.com/MerseniBilel/warren.") &&
			!strings.HasPrefix(f.Function, "github.com/MerseniBilel/warren/") {
			file := f.File
			parts := strings.Split(file, "/")
			if len(parts) > 2 {
				file = strings.Join(parts[len(parts)-2:], "/")
			}
			return file + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return "unknown location"
		}
	}
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

// telemetryType is what the bootstrapper looks for in the root scope before
// step 5: an exported app.Telemetry means instrumentation is composed into
// every route.
var telemetryType = reflect.TypeFor[app.Telemetry]()

// validatorType is the same seam for validation: a validate.Validator in the
// graph is the one every route's rules are compiled against, exactly as
// App.Validator would have set it.
//
// It exists because App.Validator is not reachable from every place an app
// is booted. warrentest.NewModuleTest calls Start itself, so a module whose
// requests carry tags the standard-library validator refuses — which is what
// installing warren/validate/playground is FOR — could be served in
// production and not booted in a test. Two shipped features were mutually
// exclusive; making the validator resolvable from the graph, as telemetry
// already was, is what reconciles them.
var validatorType = reflect.TypeFor[validate.Validator]()

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

// nilChecked wraps a constructor so a nil return is a BOOT failure rather
// than a 500 on the first request that touches it.
//
// warren.md §1.3's headline rule is that every error the framework can
// detect surfaces at boot, never on request 1 — and a provider returning a
// nil interface booted clean, logged "http server listening", and panicked
// inside the handler. It is detectable exactly here, where the value is
// constructed.
//
// It wraps only what it must: a constructor with no nilable output is
// returned untouched, a variadic one is left alone rather than reconstructed
// wrongly, and one whose every nilable output is declared Optional needs no
// wrapper at all. The wrapper's own declaration site never reaches a
// diagnostic, because the site is passed to dig explicitly as an option.
func nilChecked(ctor any, module string, optional []reflect.Type) any {
	t := reflect.TypeOf(ctor)
	if t == nil || t.Kind() != reflect.Func || t.IsVariadic() {
		return ctor
	}

	values, hadErr := valueOutputs(t)
	if !anyChecked(values, optional) {
		return ctor
	}

	ins := make([]reflect.Type, 0, t.NumIn())
	for i := range t.NumIn() {
		ins = append(ins, t.In(i))
	}
	outs := append(append([]reflect.Type{}, values...), errType)

	fn := reflect.ValueOf(ctor)
	wrapper := reflect.MakeFunc(reflect.FuncOf(ins, outs, false), func(args []reflect.Value) []reflect.Value {
		got := fn.Call(args)

		// The constructor's own error wins: it knows more than this check.
		if hadErr {
			if e := got[len(got)-1]; !e.IsNil() {
				return got
			}
			got = got[:len(got)-1]
		}
		for i, v := range got {
			if isNil(v) && !slices.Contains(optional, values[i]) {
				out := make([]reflect.Value, len(outs))
				for j := range values {
					out[j] = reflect.Zero(values[j])
				}
				out[len(outs)-1] = reflect.ValueOf(errNilProvider(module, values[i]))
				return out
			}
		}
		return append(got, reflect.Zero(errType))
	})
	return wrapper.Interface()
}

// valueOutputs splits a constructor's outputs into its values and whether it
// ended with an error.
func valueOutputs(t reflect.Type) ([]reflect.Type, bool) {
	var values []reflect.Type
	hadErr := false
	for i := range t.NumOut() {
		o := t.Out(i)
		if i == t.NumOut()-1 && o == errType {
			hadErr = true
			continue
		}
		values = append(values, o)
	}
	return values, hadErr
}

// anyChecked reports whether any output is both nilable and NOT declared
// Optional — that is, whether wrapping this constructor could ever reject
// anything. A constructor whose only nilable output is a declared absence is
// returned unwrapped, so Optional costs nothing at boot and nothing after it.
func anyChecked(ts []reflect.Type, optional []reflect.Type) bool {
	for _, t := range ts {
		switch t.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			if !slices.Contains(optional, t) {
				return true
			}
		}
	}
	return false
}

// isNil reports whether v is a nil of a nilable kind. It looks at the
// RETURNED value only: a perfectly good value carrying a nil field is none
// of boot's business.
func isNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// errOptionalWithoutProvider refuses a waiver nothing can use.
func errOptionalWithoutProvider(module, declared string, t reflect.Type) error {
	return diagnostic(fmt.Sprintf(
		"✗ optional without provider\n\n"+
			"    module %q (%s) declares warren.Optional[%s](), but none of its\n"+
			"    providers returns it.\n\n"+
			"  Optional waives the nil check for one type. A waiver for a type the\n"+
			"  module does not provide waives nothing today and hides a real nil the\n"+
			"  day someone adds that provider — and nothing would report it.\n\n"+
			"  Remove the Optional, or correct the type it names.",
		module, declared, t))
}

func errNilProvider(module string, t reflect.Type) error {
	return diagnostic(fmt.Sprintf(
		"✗ provider returned nil\n\n"+
			"    module %q\n"+
			"      └─ a constructor for %s returned nil.\n\n"+
			"  Nothing downstream can check that, so the first request reaching it\n"+
			"  panics and becomes a 500 — long after the boot that could have said so.\n\n"+
			"  Return a real value, or return an error explaining why you cannot:\n"+
			"  a constructor may return (T, error) and the boot reports it by name.",
		module, t))
}
