package di

import (
	"fmt"
	"strings"
)

// The diagnostics are the product. Every error this package returns is
// composed here, in Warren's voice; dig's wording never reaches a caller
// (invariant 2). The missing-provider block is warren.md §2.2's golden
// diagnostic and is covered byte for byte by a golden-file test.

type candidate struct {
	provider string
	scope    string
	exported bool
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
func errMissing(target string, chain []string, declared, scope string, candidates []candidate) error {
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
				fmt.Fprintf(&b, "    • %s is exported by scope %q — make this module import it.\n", c.provider, c.scope)
			} else {
				fmt.Fprintf(&b, "    • %s is registered in scope %q but not exported.\n", c.provider, c.scope)
				fmt.Fprintf(&b, "      Add to %s's module: warren.Exports[%s]()\n", c.scope, target)
			}
		}
		fmt.Fprintf(&b, "    • Or provide it locally:  warren.Providers(%s)\n", candidates[0].provider)
	}
	return &diagnostic{text: strings.TrimRight(b.String(), "\n")}
}

// errAmbiguous reports one type with more than one visible provider, naming
// every provider and where it is registered.
func errAmbiguous(t fmt.Stringer, scope string, providers []*provider) error {
	var b strings.Builder
	b.WriteString("✗ ambiguous binding\n\n")
	fmt.Fprintf(&b, "    %s has %d providers visible from scope %q:\n", t, len(providers), scope)
	for _, p := range providers {
		fmt.Fprintf(&b, "      • %s — registered in scope %q at %s\n", p.name, p.scope.name, p.site)
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

// errVariadic reports a variadic constructor, which Warren does not accept.
func errVariadic(name string) error {
	return &diagnostic{text: fmt.Sprintf(
		"✗ variadic constructor\n\n    %s is variadic. A constructor's parameters are the dependencies it\n    resolves, so each must be a single concrete requirement.", name)}
}

// errConstructorFailed presents a user constructor's own error, wrapped so
// errors.Is and errors.As still reach it.
func errConstructorFailed(scope string, cause error) error {
	return &diagnostic{
		text: fmt.Sprintf(
			"✗ constructor failed\n\n    %v\n\n  A constructor returned this error while the graph for scope %q was\n  being built. Fix the constructor, or the configuration it read.", cause, scope),
		cause: cause,
	}
}

// errInternal is the sanitized fallback for a container failure this package
// did not anticipate. It deliberately carries no third-party wording.
func errInternal(name string) error {
	return &diagnostic{text: fmt.Sprintf(
		"✗ container failure\n\n    Registration or invocation of %s failed inside the container in a way\n    Warren did not anticipate. This is a Warren bug — please report it.", name)}
}
