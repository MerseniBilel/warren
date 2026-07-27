#!/usr/bin/env bash
#
# Enforce the module invariants from ADR-0003 and ADR-0001.
#
# These are architecture rules that `warren lint arch` will own once it exists
# (ADR-0004). Until then they are enforced here, because a rule that is only
# written down is a rule that decays -- which is the problem Warren exists to
# solve, and we do not get an exemption from it.
#
# Checks:
#   1. The core module has zero third-party dependencies. Permanently.
#   2. No committed `replace` directive in any module.
#   3. No `toolchain` directive in any module (ADR-0007).
#   4. Every module declares the same Go version.
#   5. Only warren/di imports go.uber.org/dig (ADR-0001 rule 1).
#
# Usage: scripts/check-module-rules.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[0;33m'; NC=$'\033[0m'
failures=0

fail() { echo "${RED}FAIL${NC}  $*" >&2; failures=$((failures + 1)); }
pass() { echo "${GREEN}ok${NC}    $*"; }
note() { echo "${YELLOW}note${NC}  $*"; }

# Expected Go version, taken from the root module as the source of truth.
EXPECTED_GO="$(awk '/^go [0-9]/ {print $2; exit}' go.mod)"

# Portable array fill: macOS ships bash 3.2, which has no `mapfile`.
MODULES=()
while IFS= read -r line; do
  MODULES+=("$line")
done < <(find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' | sort)

echo "Checking ${#MODULES[@]} module(s) against ADR-0001, ADR-0003, ADR-0007"
echo

# ---------------------------------------------------------------------------
# 1. Core module: zero third-party dependencies.
# ---------------------------------------------------------------------------
core_requires="$(awk '
  /^require \(/ {inblock=1; next}
  inblock && /^\)/ {inblock=0; next}
  inblock && NF && $0 !~ /^[[:space:]]*\/\// {print $1}
  /^require [^(]/ {print $2}
' go.mod || true)"

if [[ -n "$core_requires" ]]; then
  fail "core module has third-party dependencies, violating ADR-0003 rule 1:"
  echo "$core_requires" | sed 's/^/        /' >&2
  echo "        The core module is stdlib-only, permanently. Define the port in" >&2
  echo "        core and put the implementation in a submodule." >&2
else
  pass "core module has zero third-party dependencies (ADR-0003 rule 1)"
fi

# ---------------------------------------------------------------------------
# 2-4. Per-module directives.
# ---------------------------------------------------------------------------
for modfile in "${MODULES[@]}"; do
  mod="${modfile#./}"; mod="${mod%/go.mod}"; [[ "$mod" == "go.mod" ]] && mod="."

  if grep -qE '^replace ' "$modfile" || grep -qE '^\s+[^ ]+ => ' "$modfile"; then
    fail "$mod: committed 'replace' directive (ADR-0003). Use go.work locally; it is git-ignored."
  fi

  if grep -qE '^toolchain ' "$modfile"; then
    fail "$mod: 'toolchain' directive (ADR-0007). A library must not pin a toolchain."
  fi

  modgo="$(awk '/^go [0-9]/ {print $2; exit}' "$modfile")"
  if [[ "$modgo" != "$EXPECTED_GO" ]]; then
    fail "$mod: declares go $modgo, expected go $EXPECTED_GO (ADR-0007: all modules track the same Go major)."
  fi
done
[[ $failures -eq 0 ]] && pass "no replace/toolchain directives; Go version consistent at $EXPECTED_GO"

# ---------------------------------------------------------------------------
# 5. ADR-0001 rule 1: dig containment.
# ---------------------------------------------------------------------------
# depguard enforces this too, but only where golangci-lint runs. This catches it
# in environments where the linter is unavailable, and it names the ADR.
dig_violations="$(grep -rEl '"go\.uber\.org/dig"' --include='*.go' . 2>/dev/null \
  | grep -v '/di/' | grep -v '^\./di/' || true)"

if [[ -n "$dig_violations" ]]; then
  fail "go.uber.org/dig imported outside warren/di (ADR-0001 rule 1):"
  echo "$dig_violations" | sed 's/^/        /' >&2
  echo "        dig must not appear in any Warren public signature. Wrap it in warren/di." >&2
else
  pass "dig is contained to warren/di (ADR-0001 rule 1)"
fi

echo
if [[ $failures -gt 0 ]]; then
  echo "${RED}$failures module rule violation(s).${NC} See docs/adr/ for the reasoning." >&2
  exit 1
fi
echo "${GREEN}All module rules satisfied.${NC}"
