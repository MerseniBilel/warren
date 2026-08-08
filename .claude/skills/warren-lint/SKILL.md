---
name: warren-lint
description: Use when checking a Warren project's architecture, when `warren lint arch` reports a violation, or when deciding how to fix a layer or cross-module import breach.
---

# Checking the architecture

```
warren lint arch [dir]
```

It reads the import graph and reports every package that breaks one of two
rules. It reads imports **syntactically**, so it works on a project that does
not compile — which is when it matters most, because the fix for a layer
violation usually breaks the build first.

**Exit codes: `0` clean · `1` violations found · `2` could not run.** The
three are distinct on purpose: a CI that cannot tell "could not analyse" from
"clean" has quietly stopped enforcing anything. Never collapse them, and
never `|| true` this command.

## The two rules

**The layer rule.** `domain` imports nothing from `application`,
`infrastructure`, or `interfaces`. The dependency arrow points inward, always.
This is what lets a domain be tested without a database, versioned on its own,
and extracted later.

**The cross-module rule.** A feature module does not reach into another
feature's internals. What crosses a module boundary is what the module
exports — a port — never a concrete type from its `infrastructure`, and never
its `domain` types pulled in sideways.

`module.go` is exempt from the layer rule: it is the composition root of its
feature and is *supposed* to see everything. Nothing else in the feature is.

## Fixing a violation, in order of preference

**domain → infrastructure.** The domain is reaching for a driver. Declare a
**port** in the domain — an interface named for what the domain needs, not for
what implements it — and put the implementation in `infrastructure`. The
generated repositories show the shape.

**domain → application.** Usually a type that is in the wrong layer. A view or
a DTO belongs in `application`; a value object or an invariant belongs in
`domain`. Move the type, do not add the import.

**feature A → feature B's internals.** Two ways out, and they are not
equivalent:

- If A genuinely needs a *capability* B owns: B exports a port with
  `warren.Exports[...]()`, and A depends on the port — **and the port's
  interface must live in a self-contained package outside
  `internal/modules/`** (`internal/contracts/<owner>/`, declaring the
  interface and its own types and importing no feature). `Exports` alone is
  not enough: to *name* the type A has to import the package declaring it,
  and this rule refuses A importing B's packages. A port left inside B is a
  port no other feature can legally reach.
- If A is reacting to something that *happened* in B: A should consume B's
  **event**, not call into it. `warren g consumer` writes that. The event is
  a wire contract, so B stays extractable into its own service — which a
  direct Go import would destroy.

## What not to do

Do not silence it. There is no ignore file and no `//nolint` for these rules
by design: they are the invariants the framework rests on, and a project that
suppresses them has the directory layout of a Warren application and none of
the properties.

Do not move the offending file into `module.go`'s package to use the
exemption. That makes the composition root do the feature's work and hides
the coupling rather than removing it.
