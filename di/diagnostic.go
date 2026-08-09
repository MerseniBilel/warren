package di

import (
	stderrors "errors"
	"fmt"
	"go/token"
	"strings"
	"unicode"
)

// The diagnostics are the product. Every error this package returns is
// composed here, in Warren's voice; dig's wording never reaches a caller
// (invariant 2). The missing-provider block is warren.md §2.2's golden
// diagnostic and is covered byte for byte by a golden-file test.

type candidate struct {
	provider string
	scope    string
	exported bool
	concrete string // the output type, when it merely IMPLEMENTS the target
}

// diagnostic is the error type every rendered block is returned as. The
// rendered text is the contract; the wrapped cause, when present, is a user
// constructor's own error.
type diagnostic struct {
	text  string
	cause error
}

func (d *diagnostic) Error() string { return d.text }

func (d *diagnostic) Unwrap() error { return d.cause }

// errMissing renders the golden missing-provider diagnostic: the requirement
// chain from the unresolved type up to the module declaration, the scope
// verdict, and what to type to fix it.
func errMissing(target string, chain []string, declared, scope string, candidates, implementers []candidate) error {
	var b strings.Builder
	b.WriteString("✗ cannot resolve dependency\n\n")
	b.WriteString("    " + target + "\n")
	indent := 6
	for _, link := range chain {
		fmt.Fprintf(&b, "%s└─ required by %s\n", strings.Repeat(" ", indent), link)
		indent += 5
	}
	if declared != "" {
		fmt.Fprintf(&b, "%s└─ declared in %s\n", strings.Repeat(" ", indent), declared)
	}
	fmt.Fprintf(&b, "\n  No provider found in scope %q or its imports.\n", scope)

	if len(candidates) > 0 {
		b.WriteString("\n  Did you mean:\n")
		for _, c := range candidates {
			if c.exported {
				// The type is already exported: the missing half is on THIS
				// side, and Imports is the line to type. Naming Exports here
				// would send the reader to the module that already has it.
				fmt.Fprintf(&b, "    • %s is exported by module %q, which %q does not import.\n", c.provider, c.scope, scope)
				// Only when the module's name is also a Go identifier can it
				// be the package qualifier. Warren's own adapters name
				// themselves by PATH — "warren/persistence/postgres" — and
				// interpolating that produced
				// warren.Imports(warren/persistence/postgres.Module()),
				// which does not parse. There the module value lives in the
				// application's own wiring under a name only the reader
				// knows, so the honest answer is to say what to do without
				// inventing an identifier.
				if isIdentifier(c.scope) {
					fmt.Fprintf(&b, "      Add to %s's module: warren.Imports(%s.Module())\n", scope, c.scope)
				} else {
					// The module VALUE, not a fresh call. A reader who
					// followed this into a second postgres.Module(...) got
					// "duplicate module name" — modules are deduplicated by
					// identity, so the second declaration is a different
					// module wearing the same name.
					fmt.Fprintf(&b, "      Add module %q to %s's warren.Imports(...) — the value that\n"+
						"      already declares it, wherever your project keeps it. Calling its\n"+
						"      constructor again makes a SECOND module with the same name, which\n"+
						"      fails the boot.\n", c.scope, scope)
				}
			} else {
				fmt.Fprintf(&b, "    • %s is registered in scope %q but not exported.\n", c.provider, c.scope)
				fmt.Fprintf(&b, "      Add to %s's module: warren.Exports[%s]()\n", c.scope, target)
				// The block used to stop there, and that made two shipped
				// tools contradict each other: a field test followed this
				// advice between two feature modules and `warren lint arch`
				// then refused the import the advice requires. Exports is
				// necessary but not sufficient, and saying so is the fix
				// (architect ruling, 2026-08-08).
				//
				// It prints unconditionally because di sees scope NAMES, not
				// import paths — reflect.Type.String() drops the path — so it
				// cannot tell a sibling feature from platform or from
				// warren/persistence/postgres. Three lines that are true in
				// every case beat a condition that guesses.
				b.WriteString("      That is the first half. To NAME the type, this module must also\n" +
					"      import the package declaring it — and `warren lint arch` refuses one\n" +
					"      feature module importing another's packages. A port shared between\n" +
					"      features belongs in a self-contained package outside\n" +
					"      internal/modules/ (internal/contracts/<owner>/), declaring the\n" +
					"      interface and its own types and importing no feature.\n" +
					"      If this module is REACTING rather than asking, consume the owner's\n" +
					"      event instead — warren g consumer — and neither half is needed.\n")
			}
		}
		// "Or provide it locally" hands back a provider NAME derived from
		// reflection, and a name is not source. platform.newUnitOfWork is
		// unexported and unreferenceable from the asking package;
		// postgres.Module.func2 is a closure and not an identifier at all.
		// Print it only when it is something the reader can actually type —
		// the suggestions above are the real fix regardless.
		if ref := candidates[0].provider; isQualifiedExported(ref) {
			fmt.Fprintf(&b, "    • Or provide it locally:  warren.Providers(%s)\n", ref)
		}
	}
	// Nothing provides the target, but something SATISFIES it. Say so — the
	// fix is one word on one line the reader is already looking at.
	if len(candidates) == 0 && len(implementers) > 0 {
		b.WriteString("\n  Did you mean:\n")
		for _, c := range implementers {
			fmt.Fprintf(&b, "    • %s returns %s,\n      which IMPLEMENTS %s.\n",
				c.provider, c.concrete, target)
			b.WriteString("      The container resolves by the DECLARED type, not by what\n" +
				"      satisfies it. Declare the constructor's return type as the port:\n")
			// Only when the name is one a reader can actually type. A closure
			// renders as "pkg.Func.func1", and printing that inside a func
			// signature is the same unpasteable-fix bug as the Imports line.
			if name := shortName(c.provider); isIdentifier(name) {
				fmt.Fprintf(&b, "          func %s(...) %s\n", name, target)
			} else {
				fmt.Fprintf(&b, "          func ...(...) %s\n", target)
			}
		}
	}
	// Nothing provides it and nothing satisfies it, so there is no candidate
	// to name — and until 2026-08-08 the block simply ended here, leaving the
	// reader with a perfect requirement chain and no next step. A field test
	// reported it as the showcased "Did you mean" having vanished, because
	// nothing says the hints above are conditional.
	//
	// There are exactly two shapes the fix can take and the container cannot
	// tell which, so it names both rather than guessing. Saying "one of these
	// two lines" beats saying nothing.
	if len(candidates) == 0 && len(implementers) == 0 {
		fmt.Fprintf(&b, "\n  Nothing in the graph provides it, so one of two lines is missing:\n\n"+
			"    • If %s belongs to this module, provide it:\n"+
			"          warren.Providers(NewWhateverReturns%s)\n\n"+
			"    • If another module owns it, import that module — and it must\n"+
			"      export the type, or the import cannot see it:\n"+
			"          warren.Imports(other.Module())   in %q\n"+
			"          warren.Exports[%s]()   in the module that provides it\n\n"+
			"  A constructor whose return type is the CONCRETE type does not\n"+
			"  satisfy this either — the container resolves by the declared type.\n",
			target, shortTypeName(target), scope, target)
	}
	return &diagnostic{text: strings.TrimRight(b.String(), "\n")}
}

// shortTypeName is the bare type name, for a suggested constructor that
// reads like something a user would type.
func shortTypeName(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return strings.TrimPrefix(target[i+1:], "*")
	}
	return strings.TrimPrefix(target, "*")
}

// shortName drops the package qualifier from a provider name, so the
// suggested signature reads as the function the user will edit.
func shortName(provider string) string {
	if _, after, ok := strings.Cut(provider, "."); ok {
		return after
	}
	return provider
}

// isIdentifier reports whether s can appear in Go source where an identifier
// is required. Module names are free-form strings — Warren's own adapters use
// paths like "warren/persistence/postgres" — so anything interpolated into a
// suggested fix has to be checked first.
func isIdentifier(s string) bool {
	if s == "" || token.IsKeyword(s) {
		return false
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

// isQualifiedExported reports whether name is a "pkg.Name" a reader could
// paste into their own file: both halves identifiers, and the second one
// exported. It rejects what reflection hands back for the two cases that
// actually reached users — an unexported constructor (platform.newUnitOfWork,
// invisible outside its package) and a closure (postgres.Module.func2, not an
// identifier at all).
func isQualifiedExported(name string) bool {
	pkg, ident, ok := strings.Cut(name, ".")
	if !ok || !isIdentifier(pkg) || !isIdentifier(ident) {
		return false
	}
	return token.IsExported(ident)
}

// errAmbiguous reports one type with more than one visible provider, naming
// every provider and where it is registered.
func errAmbiguous(t fmt.Stringer, scope string, providers []*provider) error {
	var b strings.Builder
	b.WriteString("✗ ambiguous binding\n\n")
	fmt.Fprintf(&b, "    %s has %d providers visible from scope %q:\n", t, len(providers), scope)
	for _, p := range providers {
		fmt.Fprintf(&b, "      • %s — registered in scope %q at %s\n", p.name, p.scope.name, p.declarationSite())
		// Two providers declared by the SAME module share one DeclaredAt —
		// the NewModule line — so the location above is identical on both
		// bullets and points at neither constructor. The definition site is
		// the one thing that always differs, and it is where the reader is
		// going anyway.
		if p.site != "" && p.site != p.declarationSite() {
			fmt.Fprintf(&b, "        defined at %s\n", p.site)
		}
	}
	b.WriteString("\n  Keep one, or move the extra binding into the module that needs it.")
	return &diagnostic{text: b.String()}
}

// errCycle reports a provider cycle as the loop of types, each requiring the
// next.
func errCycle(cycle []string) error {
	var b strings.Builder
	b.WriteString("✗ dependency cycle\n\n")
	b.WriteString("    " + strings.Join(cycle, " → ") + "\n")
	b.WriteString("\n  Break the loop: one of these constructors must stop depending on the other.")
	return &diagnostic{text: b.String()}
}

// errNonConstructor reports a Provide or Invoke argument that is not usable.
func errNonConstructor(got any) error {
	return &diagnostic{text: fmt.Sprintf(
		"✗ not a constructor\n\n    Provide expects a function whose returns are the types it provides,\n    optionally with a trailing error. Got %T.", got)}
}

// errConstructorFailed presents a user constructor's own error, wrapped so
// errors.Is and errors.As still reach it.
func errConstructorFailed(scope string, cause error) error {
	// Which constructor, when Provide could tag it. Naming it is the whole
	// difference between a diagnostic and a scavenger hunt: the cause is
	// often something as unplaceable as "EOF".
	who := "A constructor"
	var attr *attributedError
	if stderrors.As(cause, &attr) {
		who = attr.name + " (" + attr.site + ")"
	}
	return &diagnostic{
		text: fmt.Sprintf(
			"✗ constructor failed\n\n    %v\n\n  %s returned this error while the graph for scope %q was\n  being built. Fix the constructor, or the configuration it read.", cause, who, scope),
		cause: cause,
	}
}

// errConstructorPanicked presents a constructor that panicked rather than
// returning an error.
//
// This is the path that escaped invariant 2 for a whole release. A panic
// unwinds through dig's own frames, so a user who hit one of Warren's
// DELIBERATE refusals — app.Chain's transaction-ordering panic, say — was
// handed a stack trace whose middle nine frames were module-cache paths into
// go.uber.org/dig, immediately after ten diagnostics that had told them
// exactly what to fix. The refusal's message is excellent; the frames around
// it undid it.
//
// The panic value is reproduced verbatim, because for a Warren refusal the
// value IS the diagnostic — several paragraphs of it. The stack is kept too,
// with the container's plumbing removed: a panic is as often a genuine bug in
// the constructor (a nil map, an index) as a refusal, and a bug with no
// frames is unfindable.
func errConstructorPanicked(scope string, value any, stack string) error {
	frames := cleanStack(stack)
	who := "A constructor"
	if len(frames) > 0 {
		who = frames[0].fn
	}

	var b strings.Builder
	b.WriteString("✗ constructor panicked\n\n")
	for _, line := range strings.Split(strings.TrimRight(fmt.Sprint(value), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	fmt.Fprintf(&b, "\n  %s panicked while the graph for scope %q was being built.\n", who, scope)
	b.WriteString("  A panic during boot is one of two things: a refusal Warren raises on\n")
	b.WriteString("  purpose — the message above says so when it is — or a bug in the\n")
	b.WriteString("  constructor. Either way the process never served a request.\n")
	if len(frames) > 0 {
		b.WriteString("\n  Where it came from:\n\n")
		for _, f := range frames {
			b.WriteString("    " + f.fn + "\n        " + f.at + "\n")
		}
	}
	return &diagnostic{text: strings.TrimRight(b.String(), "\n")}
}

type stackFrame struct{ fn, at string }

// shortFuncName drops the module path from a stack frame's function, leaving
// the package and the function — "user/application.NewRegisterUserHandler"
// rather than 60 characters of forge, owner and repository that are the same
// on every line.
func shortFuncName(fn string) string {
	dot := strings.LastIndexByte(fn, '.')
	if dot < 0 {
		return fn
	}
	pkg := fn[:dot]
	if slash := strings.LastIndexByte(pkg, '/'); slash >= 0 {
		pkg = pkg[slash+1:]
	}
	return pkg + fn[dot:]
}

// cleanStack turns debug.Stack's output into the frames a user can act on.
//
// It drops three kinds of noise: the panic machinery (runtime.gopanic and the
// deferred recover that captured this), dig's frames — which invariant 2
// forbids showing — and this package's own. What is left starts at the code
// that actually panicked.
func cleanStack(stack string) []stackFrame {
	lines := strings.Split(stack, "\n")
	var out []stackFrame
	// Frames come in pairs: a function line, then a tab-indented file:line.
	for i := 0; i+1 < len(lines); i++ {
		fn := lines[i]
		at := strings.TrimSpace(lines[i+1])
		if fn == "" || !strings.HasPrefix(lines[i+1], "\t") {
			continue
		}
		i++ // the file:line belongs to this frame, never to the next
		switch {
		case strings.HasPrefix(fn, "go.uber.org/dig"), strings.Contains(at, "go.uber.org/dig"):
		case strings.HasPrefix(fn, "panic("), strings.HasPrefix(fn, "runtime/debug.Stack"):
		case strings.HasPrefix(fn, "runtime."), strings.HasPrefix(fn, "created by "):
		case strings.HasPrefix(fn, "github.com/MerseniBilel/warren/di."):
		// reflect is how the container calls a constructor at all. Those two
		// frames sit between the caller and the constructor in every single
		// trace and say nothing about this one.
		case strings.HasPrefix(fn, "reflect."):
		default:
			// The argument list and the +0x.. offset are compiler detail that
			// helps nobody reading a boot failure. Cut at the LAST paren and
			// only when one closes the line: a method's frame is printed
			// "warren.(*App).Start.func1(0x14000)", whose FIRST paren opens
			// the receiver — cutting there left the frame reading "warren."
			// and nothing else.
			if strings.HasSuffix(fn, ")") {
				if p := strings.LastIndexByte(fn, '('); p > 0 {
					fn = fn[:p]
				}
			}
			if p := strings.LastIndex(at, " +0x"); p > 0 {
				at = at[:p]
			}
			out = append(out, stackFrame{fn: shortFuncName(fn), at: at})
		}
	}
	return out
}

// errInternal is the sanitized fallback for a container failure this package
// did not anticipate. It deliberately carries no third-party wording.
func errInternal(name string) error {
	return &diagnostic{text: fmt.Sprintf(
		"✗ container failure\n\n    Registration or invocation of %s failed inside the container in a way\n    Warren did not anticipate. This is a Warren bug — please report it.", name)}
}
