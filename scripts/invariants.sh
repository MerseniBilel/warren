#!/usr/bin/env bash
# Mechanical checks for the AGENT.md invariants that grep can see.
# The ones grep cannot see (driver types in public signatures, ring imports)
# are enforced in review and by each module's own tests.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0

# Invariant 1 — the core module is stdlib + go.uber.org/dig, nothing else.
direct=$( { sed -n '/^require ($/,/^)$/p' go.mod | sed '1d;$d'; grep -E '^require [a-z]' go.mod | sed 's/^require //'; } \
	| grep -v '// indirect' | awk 'NF {print $1}' | grep -v '^go.uber.org/dig$' || true)
if [ -n "$direct" ]; then
	echo "invariant 1: core go.mod has a direct dependency beyond go.uber.org/dig:"
	echo "$direct"
	fail=1
fi

# Invariant 1, second half — and no INDIRECT requires either.
#
# The check above filters "// indirect", which is how `go work sync` quietly
# added an indirect testify require to the core go.mod on 2026-08-02 and got
# a green run. An indirect require is still a line in the core module's
# dependency graph, and today the correct count is zero: dig brings nothing
# of its own into go.mod. If that ever changes legitimately, change this
# check deliberately rather than widening the filter.
indirect=$(grep -c '// indirect' go.mod || true)
if [ "$indirect" != "0" ]; then
	echo "invariant 1: core go.mod has $indirect indirect require(s); it must have none:"
	grep '// indirect' go.mod
	echo "  If this came from 'go work sync', that command must not be run — see the Makefile."
	fail=1
fi

# Invariant 2 — dig is imported by warren/di alone.
offenders=$(grep -rl --include='*.go' 'go.uber.org/dig' . | grep -v '^\./di/' || true)
if [ -n "$offenders" ]; then
	echo "invariant 2: go.uber.org/dig imported outside warren/di:"
	echo "$offenders"
	fail=1
fi

# Ring direction — the root warren package is the kernel's composition root
# and may import the contracts ring (it drives boot step 5). No OTHER kernel
# package may: a kernel package that knows what a route is has collapsed the
# ring, and the composition-root carve-out exists to be exactly one package
# wide. The kernel is §1.1's list, minus the root package itself. Import
# lines only — a doc comment naming an adapter is not an import.
for pkg in di lifecycle config log errors validate health; do
	[ -d "$pkg" ] || continue
	leaks=$(grep -rlnE --include='*.go' \
		'^[[:space:]]*([A-Za-z_][A-Za-z0-9_]* )?"github.com/MerseniBilel/warren/(transport|persistence|broker|app)("|/)' \
		"$pkg" | grep -v '_test\.go$' || true)
	if [ -n "$leaks" ]; then
		echo "ring direction: kernel package $pkg imports the contracts ring:"
		echo "$leaks"
		echo "  Only the root warren package may — it is the composition root."
		fail=1
	fi
done

# OpenTelemetry is confined to warren/observability.
#
# It costs 24 third-party modules, including grpc and protobuf — an order of
# magnitude more than anything else in the repository — so it is opt-in, in
# its own module, and a service that does not import it must pay nothing. The
# seam is core-shaped (app.Telemetry, log.ContextAttrs), so nothing else has
# any reason to reach for the SDK; this is what stops one convenient import
# putting grpc in every user's go.sum.
otel=$(find . -name go.mod -not -path './.git/*' -not -path './observability/*' -exec grep -l 'go.opentelemetry.io' {} + 2>/dev/null || true)
if [ -n "$otel" ]; then
	echo "OpenTelemetry outside warren/observability:"
	echo "$otel"
	echo "  The seam is app.Telemetry and log.ContextAttrs — both core-shaped."
	fail=1
fi

# Invariant 8 — no committed replace directive, in any module.
replaces=$(find . -name go.mod -not -path './.git/*' -exec grep -l '^replace' {} + 2>/dev/null || true)
if [ -n "$replaces" ]; then
	echo "invariant 8: replace directive committed in:"
	echo "$replaces"
	fail=1
fi

# AGENT.md § Naming — no type named XWithY.
withtypes=$(grep -rnE --include='*.go' 'type [A-Za-z0-9]+With[A-Z][A-Za-z0-9]* ' . || true)
if [ -n "$withtypes" ]; then
	echo "naming: type name contains 'With':"
	echo "$withtypes"
	fail=1
fi

# Documentation drift — warren.md is the manifest, and a manifest that
# contradicts the code is worse than no manifest. A field test read §7.5,
# wrote a test against `require.NoError` and an Invoke argument order that
# does not exist, and reasonably concluded the docs were untrustworthy.
#
# Only the CHECKABLE claims are enforced here: an assertion library core
# does not have, and a Docker fixture library no module imports. Both are
# statements about the dependency graph, which grep can see; the rest of the
# manifest binds in review.
for lib in stretchr/testify testcontainers-go; do
	# A DIRECT require is adoption; an "// indirect" line is not. testify
	# reaches transport/http's module graph through dig's own tests
	# (go mod why -m: warren/di -> dig -> dig.test -> testify), which puts
	# it in a user's go.sum and in no user's binary. Counting that as
	# adoption would let the manifest claim a vendor nobody chose.
	if grep -h "$lib" go.mod */go.mod */*/go.mod 2>/dev/null | grep -qv '// indirect'; then
		continue
	fi
	claims=$(grep -nE "^\*\*Vendors\*\*.*$lib|^\| .*\| .*$lib.*\| Vendor" warren.md || true)
	if [ -n "$claims" ]; then
		echo "doc drift: warren.md claims $lib is vendored, but no go.mod requires it:"
		echo "$claims"
		fail=1
	fi
done

exit $fail
