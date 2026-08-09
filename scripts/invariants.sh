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
#
# Two checks, because "imports dig" and "names dig" are different failures and
# both matter. An IMPORT line outside warren/di is the invariant itself; a
# MENTION in shipped code is how dig's wording leaks into a diagnostic, which
# is the half a user actually sees.
#
# Amended 2026-08-09, when panic containment moved into warren/internal/panics.
# The old check was one grep for the string in any .go file outside ./di/, and
# it fired on the two places that ENFORCE the invariant rather than break it:
# internal/panics' frame filter, whose whole job is to drop go.uber.org/dig
# frames before a user sees them, and the tests across lifecycle, the root
# package and internal/panics that assert a rendered diagnostic contains no
# dig wording. Asserting an absence requires writing the absent thing down.
# Narrowing this to imports plus non-test mentions keeps every failure the
# check was written to catch: no package outside warren/di may import dig, and
# no shipped file outside warren/di and warren/internal/panics may name it.
imports=$(grep -rlE --include='*.go' '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]* )?"go\.uber\.org/dig"$' . | grep -v '^\./di/' || true)
if [ -n "$imports" ]; then
	echo "invariant 2: go.uber.org/dig imported outside warren/di:"
	echo "$imports"
	fail=1
fi

mentions=$(grep -rl --include='*.go' 'go.uber.org/dig' . \
	| grep -v '^\./di/' | grep -v '^\./internal/panics/' | grep -v '_test\.go$' || true)
if [ -n "$mentions" ]; then
	echo "invariant 2: go.uber.org/dig named in shipped code outside warren/di:"
	echo "$mentions"
	echo "  Only warren/di wraps it, and only warren/internal/panics may name it —"
	echo "  its frame filter exists to drop dig's frames before a user sees them."
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

# §7.3 — the resilience module was DROPPED on 2026-08-05, and the reason is
# structural: a breaker guards an OUTBOUND dependency and Warren ships no
# outbound client. These belong in the USER's infrastructure adapter, never in
# a Warren module. This check is what stops the module being re-derived.
# DIRECT requires only: an indirect one is somebody else's transitive choice
# (OpenTelemetry pulls cenkalti/backoff/v5, for instance), and this invariant
# is about deliberate adoption, not about the whole graph. Both go.mod forms
# are matched — the single-line `require X v1` and the indented block entry.
dropped='github\.com/sony/gobreaker|github\.com/cenkalti/backoff|github\.com/failsafe-go/failsafe-go|golang\.org/x/time'
resilience=$(for f in $(find . -name go.mod -not -path './.git/*'); do
	if grep -E "(^require |^[[:space:]]+)($dropped)" "$f" | grep -qv '// indirect'; then
		echo "$f"
	fi
done)
if [ -n "$resilience" ]; then
	echo "§7.3: a dropped-resilience dependency was adopted in:"
	echo "$resilience"
	echo "  Retry and timeout are core: app.Retrying, app.Timeout, broker.ExponentialBackoff."
	echo "  A breaker or limiter belongs in your infrastructure adapter, not in Warren."
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

# doc drift — a **Surface** block must list the package's WHOLE exported
# function surface.
#
# CLAUDE.md says warren.md "already fixes the public API", and a contributor
# reads the block as the contract. On 2026-08-05 the Kafka block listed a
# StatementTimeout this package has never had (copied from Postgres), omitted
# AutoCreateTopics, and said "there is no SASL" three exported SASL functions
# later; di.Named and log.Handler had shipped without ever reaching a block.
#
# Only ONE direction is mechanical. A function in the code and not in the
# block is always drift. The reverse is NOT: §2.4's block names
# config/yaml's File, a module deliberately not built yet, and a manifest is
# allowed to describe what is coming. That direction stays a review matter.
#
# Narrow by design: only sections that CHOSE to publish a Surface block are
# held to completeness. Packages documented in prose are not forced into a
# block by this check — that is an editorial decision, not a mechanical one.
for pkg in $(awk '
	function flush() { if (cur != "" && has) print cur }
	/^### / {
		flush(); cur=""; has=0
		if (match($0, /`warren[a-z0-9\/]*`/)) cur = substr($0, RSTART+1, RLENGTH-2)
		next
	}
	/^\*\*Surface\*\*/ { has=1 }
	END { flush() }
' warren.md); do
	dir=".${pkg#warren}"
	[ -d "$dir" ] || continue
	declared=$(awk -v want="$pkg" '
		/^### / {
			insec = (match($0, /`warren[a-z0-9\/]*`/) && substr($0, RSTART+1, RLENGTH-2) == want)
			seen = 0; inblk = 0; next
		}
		insec && /^\*\*Surface\*\*/ { seen = 1; next }
		seen && !inblk && /^```/ { inblk = 1; next }
		inblk && /^```/ { inblk = 0; next }
		inblk { print }
	' warren.md | grep -oE '^func [A-Z][A-Za-z0-9_]*' | awk '{print $2}' | sort -u)
	real=$(go doc -all "$dir" 2>/dev/null | grep -oE '^func [A-Z][A-Za-z0-9_]*' | awk '{print $2}' | sort -u)
	undocumented=$(comm -13 <(echo "$declared") <(echo "$real"))
	if [ -n "$undocumented" ]; then
		echo "doc drift: $pkg exports functions its warren.md Surface block omits:"
		echo "$undocumented" | sed 's/^/  /'
		fail=1
	fi
done

exit $fail
