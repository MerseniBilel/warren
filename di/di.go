// Package di is Warren's dependency-injection container: constructors are
// registered against the type they provide, the graph is validated before
// anything is built, and the result is available as data.
//
// A module registers what it provides, naming the port on the left and the
// implementation on the right:
//
//	di.Provide[domain.OrderRepository](c, postgres.NewOrderRepository)
//	di.Provide[*app.CreateOrder](c, app.NewCreateOrder)
//	di.Contribute[app.Middleware](c, NewAuthMiddleware)
//
// A constructor's parameters are its dependencies, resolved by type, so
// app.NewCreateOrder(domain.OrderRepository) needs no wiring of its own.
//
// The graph is then checked and built:
//
//	if err := c.Build(ctx); err != nil {
//		return err   // every message names a file and a fix; see SPEC.md §6
//	}
//
// [Container.Validate] reports every problem without constructing anything, so a
// failed boot has opened no connections. [Container.Build] validates and then
// constructs every singleton in dependency order — which is also what makes
// warren/lifecycle's reverse-order shutdown correct, since hooks registered
// during construction are already in dependency order.
//
// Reflection is used at registration and at Build, and nowhere else: after Build
// returns, the instance map is never written again, so [Resolve] is a map lookup
// and a type assertion and needs no lock.
package di

import (
	"context"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// callerSkip is the number of stack frames between callerSite and the code that
// called a registration function: callerSite itself, and the registration
// function it was called from.
const callerSkip = 2

// A constructor returns either the value, or the value and an error.
const (
	valueOnly     = 1
	valueAndError = 2
)

// errorType and contextType are the two types the container reasons about
// directly: the one a constructor may not provide, and the one it satisfies
// itself. They are functions rather than package-level variables because
// reflect.TypeFor is resolved from the type argument, so calling it costs nothing
// and the package keeps no state.
func errorType() reflect.Type { return reflect.TypeFor[error]() }

func contextType() reflect.Type { return reflect.TypeFor[context.Context]() }

// Container holds the registered providers and, after Build, the singletons
// constructed from them. Its zero value is not usable; construct one with [New].
//
// Registration is not safe for concurrent use: a service registers from one
// goroutine at boot. Resolution after Build is read-only and is safe for
// concurrent use without a lock.
// The field order is chosen for the garbage collector rather than for reading, in
// the manner of errors.Error: the maps come first because every word of a map
// header is a pointer, and the slices and the bool go last because their trailing
// words are not, which shrinks the scanned region from 112 bytes to 96.
type Container struct {
	single  map[reflect.Type]*provider
	grouped map[reflect.Type][]*provider
	// instances holds what Resolve reads, keyed by the registered type. made holds
	// the same values keyed by provider, which is what a group member needs since
	// its type is shared.
	instances map[reflect.Type]any
	made      map[*provider]any
	// byCtor memoises a construction against its constructor rather than against
	// the type it was registered as, so one constructor registered against two
	// ports yields one instance. See SPEC.md §5.4.
	byCtor   map[uintptr]any
	groups   map[reflect.Type]any
	buildErr error
	// order is every provider in registration order, which fixes both the order of
	// a group and the order errors are reported in.
	order   []*provider
	regErrs []error
	built   bool
}

// New returns an empty Container.
func New() *Container {
	return &Container{
		single:    make(map[reflect.Type]*provider),
		grouped:   make(map[reflect.Type][]*provider),
		instances: make(map[reflect.Type]any),
		made:      make(map[*provider]any),
		byCtor:    make(map[uintptr]any),
		groups:    make(map[reflect.Type]any),
	}
}

// provider is one registration.
type provider struct {
	typ  reflect.Type
	ctor reflect.Value
	// value holds a Supply's instance. It is unused for a constructor.
	value any
	deps  []reflect.Type
	// site is where Provide, Supply, or Contribute was called; ctorSite is where
	// the constructor itself is declared.
	site     Site
	ctorSite Site
	kind     Kind
	// id identifies the constructor for memoisation, and is zero when the
	// constructor is a closure — see memoID.
	id uintptr
}

// Kind classifies how a provider was registered.
type Kind uint8

const (
	// KindProvided was registered by Provide.
	KindProvided Kind = iota
	// KindContributed was registered by Contribute, as one member of a group.
	KindContributed
	// KindSupplied was registered by Supply, already constructed.
	KindSupplied
)

// String returns the kind's name, for example "Provided". A value outside the
// defined set renders as "Kind(7)".
func (k Kind) String() string {
	switch k {
	case KindProvided:
		return "Provided"
	case KindContributed:
		return "Contributed"
	case KindSupplied:
		return "Supplied"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Site is a position in the source, used to name a file in an error message.
type Site struct {
	Func string
	File string
	Line int
}

// String returns "file:line", or "" when the site is unknown.
func (s Site) String() string {
	if s.File == "" {
		return ""
	}

	return s.File + ":" + strconv.Itoa(s.Line)
}

// Option configures one registration.
type Option func(*provider)

// At overrides the registration site recorded for a provider.
//
// It exists for a wrapper that re-exports registration: without it, a
// warren.Provide calling [Provide] would record a file inside Warren, and every
// message in SPEC.md §6 would name the framework instead of the user's module.
// Use it with [Caller]:
//
//	func Provide[T any](c *di.Container, ctor any) {
//		di.Provide[T](c, ctor, di.At(di.Caller(1)))
//	}
func At(site Site) Option {
	return func(p *provider) { p.site = site }
}

// Caller returns the [Site] of the function skip frames above the caller of
// Caller. Caller(0) is the caller itself, Caller(1) its caller.
func Caller(skip int) Site { return callerSite(skip + callerSkip) }

// Provide registers ctor as the constructor for T.
//
// ctor is a function returning either (R) or (R, error), where R is assignable
// to T. Its parameters are its dependencies, resolved by type. Naming T
// explicitly is what lets a concrete constructor register against the port it
// satisfies, which is Warren's central pattern:
//
//	di.Provide[domain.OrderRepository](c, postgres.NewOrderRepository)
//
// A constructor is called at most once, however many types it satisfies. A
// second Provide for the same T is an error reported by [Container.Validate],
// not a silent replacement.
//
// Provide returns nothing: a malformed registration is recorded against its call
// site and reported by Validate, which keeps a module's wiring free of error
// handling on every line. See SPEC.md §5.2.
func Provide[T any](c *Container, ctor any, opts ...Option) {
	c.register(reflect.TypeFor[T](), ctor, KindProvided, callerSite(callerSkip), opts)
}

// Contribute registers ctor as one member of the group of T, resolved together
// by [Group]. Many providers may contribute the same type; that is the whole
// difference between Contribute and [Provide]:
//
//	di.Contribute[app.Middleware](c, NewAuthMiddleware)
//
// A type is provided singly or contributed to a group, never both; mixing them
// is an error reported by [Container.Validate].
func Contribute[T any](c *Container, ctor any, opts ...Option) {
	c.register(reflect.TypeFor[T](), ctor, KindContributed, callerSite(callerSkip), opts)
}

// Supply registers an already-constructed value as the instance for T, for
// something built before the container exists — configuration, most often:
//
//	di.Supply[*Config](c, cfg)
//
// A nil value is rejected, for the reason a constructor returning nil is: a nil
// dependency is a panic on the first request that reaches it.
func Supply[T any](c *Container, value T, opts ...Option) {
	typ := reflect.TypeFor[T]()
	p := &provider{typ: typ, kind: KindSupplied, value: value, site: callerSite(callerSkip)}

	for _, opt := range opts {
		opt(p)
	}

	if isNil(reflect.ValueOf(value)) {
		c.regErrs = append(c.regErrs, errSuppliedNil(p))

		return
	}

	c.add(p)
}

// register validates a constructor's shape immediately and records either the
// provider or the reason it is unusable.
func (c *Container) register(typ reflect.Type, ctor any, kind Kind, site Site, opts []Option) {
	p := &provider{typ: typ, kind: kind, site: site}

	for _, opt := range opts {
		opt(p)
	}

	err := p.bind(ctor)
	if err != nil {
		c.regErrs = append(c.regErrs, err)

		return
	}

	c.add(p)
}

// add files a well-formed provider under its type, or appends it to its group.
func (c *Container) add(p *provider) {
	c.order = append(c.order, p)

	if p.kind == KindContributed {
		c.grouped[p.typ] = append(c.grouped[p.typ], p)

		return
	}

	// A duplicate is kept rather than dropped, so that Validate can name both
	// sites. The first registration stays the one that would be built.
	if _, exists := c.single[p.typ]; !exists {
		c.single[p.typ] = p
	}
}

// bind reads ctor's signature onto p, rejecting every shape SPEC.md §5.3 lists.
func (p *provider) bind(ctor any) error {
	if p.typ == errorType() {
		return errProvidesError(p)
	}

	v := reflect.ValueOf(ctor)

	if ctor == nil || v.Kind() != reflect.Func {
		return errNotFunc(p, ctor)
	}

	ft := v.Type()

	if ft.IsVariadic() {
		return errVariadic(p, ft)
	}

	err := p.bindReturns(ft)
	if err != nil {
		return err
	}

	deps := make([]reflect.Type, 0, ft.NumIn())

	for dep := range ft.Ins() {
		if slices.Contains(deps, dep) {
			return errDuplicateParam(p, ft, dep)
		}

		deps = append(deps, dep)
	}

	p.ctor, p.deps, p.ctorSite = v, deps, funcSite(v)
	p.id = memoID(v)

	return nil
}

// bindReturns checks that ctor returns (R) or (R, error), with R satisfying the
// registered type.
func (p *provider) bindReturns(ft reflect.Type) error {
	switch ft.NumOut() {
	case valueOnly:
	case valueAndError:
		if ft.Out(1) != errorType() {
			return errReturnShape(p, ft)
		}
	default:
		return errReturnShape(p, ft)
	}

	if out := ft.Out(0); !out.AssignableTo(p.typ) {
		return errNotSatisfied(p, ft, out)
	}

	return nil
}

// memoID returns the identity a construction is memoised against, and zero when
// the constructor is a closure.
//
// reflect's own documentation warns that a func value's pointer is "not
// necessarily enough to identify a single function uniquely": every closure
// created from one source location shares a code pointer while capturing
// different variables. Deduplicating those would hand one caller another's
// instance, so closures are never deduplicated — two closures are two
// constructors even when the pointer says otherwise. A named function or method
// has no ".funcN" segment in its name and is safe to memoise, which is the case
// Warren's own pattern relies on:
//
//	di.Provide[domain.OrderReader](c, postgres.NewOrderRepository)
//	di.Provide[domain.OrderWriter](c, postgres.NewOrderRepository)  // one instance
func memoID(v reflect.Value) uintptr {
	pc := v.Pointer()

	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return 0
	}

	if isClosure(fn.Name()) {
		return 0
	}

	return pc
}

// isClosure reports whether a runtime function name denotes a closure. The
// compiler names them "pkg.Outer.func1", including nested forms such as
// "pkg.Outer.func1.2".
func isClosure(name string) bool {
	for part := range strings.SplitSeq(name, ".") {
		rest, ok := strings.CutPrefix(part, "func")
		if !ok {
			continue
		}

		_, err := strconv.Atoi(rest)
		if err == nil {
			return true
		}
	}

	return false
}

// funcSite reports where a function is declared.
func funcSite(v reflect.Value) Site {
	pc := v.Pointer()

	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return Site{}
	}

	file, line := fn.FileLine(pc)

	return Site{Func: fn.Name(), File: file, Line: line}
}

// callerSite reports the site skip frames above callerSite itself.
func callerSite(skip int) Site {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return Site{}
	}

	site := Site{File: file, Line: line}

	if fn := runtime.FuncForPC(pc); fn != nil {
		site.Func = fn.Name()
	}

	return site
}

// isNil reports whether v is a nil of a type that can be nil. A constructor
// returning one, or a Supply of one, is rejected: SPEC.md §6.9.
func isNil(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}

	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	case reflect.Array, reflect.Bool, reflect.Complex64, reflect.Complex128,
		reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64, reflect.Invalid, reflect.String,
		reflect.Struct, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return false
	default:
		return false
	}
}
