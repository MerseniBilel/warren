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

# Invariant 2 — dig is imported by warren/di alone.
offenders=$(grep -rl --include='*.go' 'go.uber.org/dig' . | grep -v '^\./di/' || true)
if [ -n "$offenders" ]; then
	echo "invariant 2: go.uber.org/dig imported outside warren/di:"
	echo "$offenders"
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

exit $fail
