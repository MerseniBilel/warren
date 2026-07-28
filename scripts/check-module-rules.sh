#!/usr/bin/env bash
#
# Enforce Warren's module invariants. See docs/architecture.md.
#
# These are architecture rules that `warren lint arch` will own once it exists.
# Until then they are enforced here, because a rule that is only
# written down is a rule that decays -- which is the problem Warren exists to
# solve, and we do not get an exemption from it.
#
# Checks:
#   1. The core module has zero third-party dependencies. Permanently.
#   2. No committed `replace` directive in any module.
#   3. No `toolchain` directive in any module.
#   4. Every module declares the same Go version.
#   5. No third-party DI container is imported anywhere -- Warren writes its own.
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

echo "Checking ${#MODULES[@]} module(s) against the architecture invariants"
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
  fail "core module has third-party dependencies:"
  echo "$core_requires" | sed 's/^/        /' >&2
  echo "        The core module is stdlib-only, permanently. Define the port in" >&2
  echo "        core and put the implementation in a submodule." >&2
else
  pass "core module has zero third-party dependencies"
fi

# ---------------------------------------------------------------------------
# 2-4. Per-module directives.
# ---------------------------------------------------------------------------
for modfile in "${MODULES[@]}"; do
  mod="${modfile#./}"; mod="${mod%/go.mod}"; [[ "$mod" == "go.mod" ]] && mod="."

  if grep -qE '^replace ' "$modfile" || grep -qE '^\s+[^ ]+ => ' "$modfile"; then
    fail "$mod: committed 'replace' directive. Use go.work locally; it is git-ignored."
  fi

  if grep -qE '^toolchain ' "$modfile"; then
    fail "$mod: 'toolchain' directive. A library must not pin a toolchain."
  fi

  modgo="$(awk '/^go [0-9]/ {print $2; exit}' "$modfile")"
  if [[ "$modgo" != "$EXPECTED_GO" ]]; then
    fail "$mod: declares go $modgo, expected go $EXPECTED_GO -- all modules track the same Go major."
  fi
done
[[ $failures -eq 0 ]] && pass "no replace/toolchain directives; Go version consistent at $EXPECTED_GO"

# ---------------------------------------------------------------------------
# 5. No third-party DI container, anywhere.
# ---------------------------------------------------------------------------
# warren/di is written in-house and lives in the core module, so this is not a
# containment rule with an exempt directory -- it is a flat ban. depguard
# enforces it too, but only where golangci-lint runs. This catches it in
# environments where the linter is unavailable.
di_violations="$(grep -rEl '"(go\.uber\.org/(dig|fx)|github\.com/samber/do)"' \
  --include='*.go' . 2>/dev/null || true)"

if [[ -n "$di_violations" ]]; then
  fail "third-party DI container imported:"
  echo "$di_violations" | sed 's/^/        /' >&2
  echo "        Warren writes its own container in warren/di, which is core and" >&2
  echo "        therefore stdlib-only. See docs/architecture.md §3." >&2
else
  pass "no third-party DI container imported"
fi

echo
if [[ $failures -gt 0 ]]; then
  echo "${RED}$failures module rule violation(s).${NC} See docs/architecture.md for the reasoning." >&2
  exit 1
fi
echo "${GREEN}All module rules satisfied.${NC}"
