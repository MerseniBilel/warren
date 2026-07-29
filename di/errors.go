package di

import (
	"reflect"
	"slices"
	"strings"

	"github.com/MerseniBilel/warren/errors"
)

// The operation recorded on an error is the public entry point that failed, not
// the internal walk that found it.
const (
	opValidate = "di.Validate"
	opBuild    = "di.Build"
	opResolve  = "di.Resolve"
	opGroup    = "di.Group"
)

// Field keys shared by the messages in SPEC.md §6. They read as prose because
// Detail renders them to a developer in a terminal.
const (
	keyRequestedBy = "requested by"
	keyChain       = "chain"
	keyProvided    = "provided"
	keyRegistered  = "registered"
	keyConstructor = "constructor"
	keySignature   = "signature"
	keyGot         = "got"
	keyMissing     = "missing"
	keyFirst       = "first"
	keySecond      = "second"
	keyContributed = "contributed"
)

// errNoProvider is SPEC.md §6.1, and §6.12 when the registry holds a near miss.
func errNoProvider(op string, want reflect.Type, chain []reflect.Type, by Site, near *provider) *errors.Error {
	e := errors.Invalid("no provider for %s", typeName(want)).Op(op)

	if by.File != "" {
		e = e.Field(keyRequestedBy, by.String())
	}

	if len(chain) > 0 {
		e = e.Field(keyChain, chainString(chain))
	}

	if near == nil {
		return e.Fix("add di.Provide[%s](c, %s) to %s", typeName(want), ctorHint(want), byFile(by))
	}

	return e.Field(keyProvided, typeName(near.typ)+", at "+near.site.String()).
		Fix("depend on %s, or register %s with di.Provide[%s](c, %s)",
			typeName(near.typ), typeName(want), typeName(want), ctorHint(want))
}

// errProvidedTwice is SPEC.md §6.2.
func errProvidedTwice(first, second *provider) *errors.Error {
	return errors.Invalid("%s is provided twice", typeName(first.typ)).
		Op(opValidate).
		Field(keyFirst, first.site.String()).
		Field(keySecond, second.site.String()).
		Fix("remove one of the two di.Provide[%s] calls, or give one its own type, as in: type ReplicaDB struct{ %s }",
			typeName(first.typ), typeName(first.typ))
}

// errCycle is SPEC.md §6.3. The path starts and ends on the same type, and each
// hop names its constructor and file because a cycle is navigated by opening
// files rather than by reading type names.
func errCycle(op string, path []*provider) *errors.Error {
	e := errors.Invalid("dependency cycle through %s", typeName(path[0].typ)).Op(op)

	for _, p := range path {
		e = e.Field(typeName(p.typ), shortFunc(p.ctorSite.Func)+" ("+p.ctorSite.String()+")")
	}

	return e.Fix("depend on an interface declared in domain/ and provide it from infrastructure/, or split %s",
		typeName(path[0].typ))
}

// errNotFunc is SPEC.md §6.4, covering an untyped nil as well as a value that
// was passed where its constructor was meant.
func errNotFunc(p *provider, ctor any) *errors.Error {
	got := "nil"

	if v := reflect.ValueOf(ctor); v.IsValid() {
		got = typeName(v.Type())
	}

	return errors.Invalid("the constructor for %s is not a function", typeName(p.typ)).
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Field(keyGot, got).
		Fix("pass the constructor, not its result: di.Provide[%s](c, %s)", typeName(p.typ), ctorHint(p.typ))
}

// errNotSatisfied is SPEC.md §6.5. It names the first missing method, which is
// the difference between a developer reading the message and reading the
// interface.
func errNotSatisfied(p *provider, ft, out reflect.Type) *errors.Error {
	e := errors.Invalid("%s does not provide %s", typeName(ft), typeName(p.typ)).
		Op(opValidate).
		Field(keyRegistered, p.site.String())

	if missing := missingMethod(p.typ, out); missing != "" {
		e = e.Field(keyMissing, missing)

		return e.Fix("implement %s on %s, or register the concrete type", methodName(missing), typeName(out))
	}

	return e.Fix("return %s, or register %s instead", typeName(p.typ), typeName(out))
}

// errReturnShape is SPEC.md §6.6.
func errReturnShape(p *provider, ft reflect.Type) *errors.Error {
	name := typeName(p.typ)

	return errors.Invalid("the constructor for %s must return (%s) or (%s, error)", name, name, name).
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Field(keySignature, typeName(ft)).
		Fix("return an error as the second value, or nothing as the second value")
}

// errVariadic is SPEC.md §6.7. dig accepts a variadic constructor and passes
// nothing, which hides a missing provider; this rejects it instead.
func errVariadic(p *provider, ft reflect.Type) *errors.Error {
	return errors.Invalid("the constructor for %s must not be variadic", typeName(p.typ)).
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Field(keySignature, typeName(ft)).
		Fix("take a slice and resolve it with di.Group, or contribute the members with di.Contribute")
}

// errProvidedAndContributed is SPEC.md §6.8.
func errProvidedAndContributed(provided, contributed *provider) *errors.Error {
	return errors.Invalid("%s is both provided and contributed to a group", typeName(provided.typ)).
		Op(opValidate).
		Field(keyProvided, provided.site.String()).
		Field(keyContributed, contributed.site.String()).
		Fix("a type is resolved by di.Resolve or by di.Group, never both — pick one")
}

// errDuplicateParam is SPEC.md §6.14.
func errDuplicateParam(p *provider, ft, dep reflect.Type) *errors.Error {
	return errors.Invalid("the constructor for %s takes two parameters of type %s",
		typeName(p.typ), typeName(dep)).
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Field(keySignature, typeName(ft)).
		Fix("one parameter is enough — the container resolves one instance per type")
}

// errProvidesError is SPEC.md §6.15. A constructor returning error cannot be
// read: the return is either the value or the failure, and nothing says which.
func errProvidesError(p *provider) *errors.Error {
	return errors.Invalid("error cannot be provided").
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Fix("a constructor's error is its second return value, not the type it provides")
}

// errReturnedNil is SPEC.md §6.9.
func errReturnedNil(p *provider) *errors.Error {
	return errors.Invalid("the constructor for %s returned nil", typeName(p.typ)).
		Op(opBuild).
		Field(keyConstructor, p.ctorSite.String()).
		Fix("return an error describing why, so that boot fails with a cause")
}

// errSuppliedNil is SPEC.md §6.9, for Supply rather than a constructor.
func errSuppliedNil(p *provider) *errors.Error {
	return errors.Invalid("the value supplied for %s is nil", typeName(p.typ)).
		Op(opValidate).
		Field(keyRegistered, p.site.String()).
		Fix("supply a value, or provide a constructor that can report why there is none")
}

// errConstructorFailed is SPEC.md §6.10. It is Internal, not Invalid: the graph
// was correct and the caller cannot fix it from the wiring.
func errConstructorFailed(p *provider, cause error) *errors.Error {
	return errors.Internal("constructing %s failed", typeName(p.typ)).
		Op(opBuild).
		Field(keyConstructor, p.ctorSite.String()).
		Wrapping(cause).
		Fix("read the cause above: it is the constructor's own failure, not a wiring mistake")
}

// errPanicked is SPEC.md §6.13. The recovered value is rendered into the message
// rather than attached as a field: it is the one thing a reader needs, and a
// field would repeat it.
func errPanicked(p *provider, recovered any) *errors.Error {
	return errors.Internal("the constructor for %s panicked: %v", typeName(p.typ), recovered).
		Op(opBuild).
		Field(keyConstructor, p.ctorSite.String()).
		Fix("this is a bug in the constructor, not in the wiring")
}

// errNotBuilt is SPEC.md §6.11.
func errNotBuilt(op string) *errors.Error {
	return errors.Invalid("the container is not built").
		Op(op).
		Fix("call Build before resolving. warren.Run does this for you — a service should not need to")
}

// errIsGroup and errIsSingle are the two group mixups, sharing §6.8's reading:
// a type is resolved one way or the other, never both.
func errIsGroup(want reflect.Type) *errors.Error {
	return errors.Invalid("%s is contributed to a group, not provided singly", typeName(want)).
		Op(opResolve).
		Fix("use di.Group[%s](c, &target)", typeName(want))
}

func errIsSingle(want reflect.Type) *errors.Error {
	return errors.Invalid("%s is provided singly, not contributed to a group", typeName(want)).
		Op(opGroup).
		Fix("use di.Resolve[%s](c, &target)", typeName(want))
}

// typeName renders a type as it is written in Go, for example "*sql.DB".
func typeName(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}

	return t.String()
}

// shortFunc renders a function's name as a reader writes it, dropping the module
// path that runtime.FuncForPC includes: "orders.NewOrderService" rather than
// "github.com/acme/shop/internal/modules/orders.NewOrderService". The full name
// stays in [Site.Func], which is data for tooling rather than prose for a person.
func shortFunc(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}

	return name
}

// chainString renders a resolution chain outermost first.
func chainString(chain []reflect.Type) string {
	parts := make([]string, 0, len(chain))

	for _, t := range chain {
		parts = append(parts, typeName(t))
	}

	return strings.Join(parts, " → ")
}

// ctorHint guesses the constructor's name from the type, so that the fix line is
// closer to something a reader can paste: *sql.DB suggests NewDB.
func ctorHint(t reflect.Type) string {
	name := typeName(t)

	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}

	name = strings.TrimLeft(name, "*[]")

	if name == "" {
		return "NewValue"
	}

	return "New" + name
}

// byFile names the file a fix should be applied to, falling back to a phrase
// rather than printing an empty path.
func byFile(by Site) string {
	if by.File == "" {
		return "a module's Register method"
	}

	return by.File
}

// missingMethod returns the first method of want, an interface, that out does
// not implement — rendered as it is declared. It returns "" when want is not an
// interface, or when the mismatch is not a method at all.
func missingMethod(want, out reflect.Type) string {
	if want.Kind() != reflect.Interface {
		return ""
	}

	for method := range want.Methods() {
		got, ok := out.MethodByName(method.Name)
		if !ok {
			return method.Name + strings.TrimPrefix(typeName(method.Type), "func")
		}

		if !sameSignature(method.Type, got.Type) {
			return method.Name + strings.TrimPrefix(typeName(method.Type), "func")
		}
	}

	return ""
}

// sameSignature compares an interface method's type with a concrete method's,
// whose first parameter is the receiver.
func sameSignature(want, got reflect.Type) bool {
	if got.NumIn() == 0 {
		return false
	}

	in := make([]reflect.Type, 0, got.NumIn()-1)

	for i := 1; i < got.NumIn(); i++ {
		in = append(in, got.In(i))
	}

	out := slices.Collect(got.Outs())

	return reflect.FuncOf(in, out, got.IsVariadic()) == want
}

// methodName strips a rendered signature back to the method's name, for a fix
// line that reads "implement Save on …".
func methodName(rendered string) string {
	if i := strings.Index(rendered, "("); i > 0 {
		return rendered[:i]
	}

	return rendered
}
