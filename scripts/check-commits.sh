#!/usr/bin/env bash
#
# Enforce the Conventional Commits format from ADR-0005.
#
# Scope matters here more than in a single-module repo: the scope is the module
# path, and release tooling uses it to decide what to bump. A wrong scope means
# a wrong release.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:-${GITHUB_BASE_REF:-main}}"

TYPES="feat|fix|perf|refactor|docs|test|build|ci|chore|revert"

# Valid scopes are module paths (ADR-0005), plus a few non-module areas.
# Keep in sync with CONTRIBUTING.md.
SCOPES="core|di|lifecycle|config|log|errors|domain|app|validate|health"
SCOPES="$SCOPES|transport|transport/http|transport/http/stdlib|transport/http/echo|transport/http/gin|transport/http/fiber"
SCOPES="$SCOPES|transport/grpc|transport/gateway|openapi"
SCOPES="$SCOPES|broker|broker/kafka|broker/rabbitmq|broker/nats|broker/memory|outbox|inbox"
SCOPES="$SCOPES|persistence|persistence/postgres|persistence/mysql|persistence/mongo|persistence/redis"
SCOPES="$SCOPES|observability|auth|resilience|jobs|testing|cli|mcp"
SCOPES="$SCOPES|docs|skills|ci|build|deps|release"

if ! git rev-parse --verify "$BASE_REF" >/dev/null 2>&1; then
  echo "note: base ref '$BASE_REF' not found; skipping commit check"
  exit 0
fi

MERGE_BASE="$(git merge-base HEAD "$BASE_REF")"
failures=0
checked=0

while IFS= read -r sha; do
  [[ -z "$sha" ]] && continue
  subject="$(git log -1 --format=%s "$sha")"
  body="$(git log -1 --format=%b "$sha")"
  short="$(git log -1 --format=%h "$sha")"
  checked=$((checked + 1))

  # Merge commits are generated, not authored.
  if [[ "$(git rev-list --parents -n1 "$sha" | wc -w)" -gt 2 ]]; then
    continue
  fi

  if [[ ! "$subject" =~ ^($TYPES)(\([a-z0-9/._-]+\))?!?:\ .+ ]]; then
    echo "FAIL $short: subject does not match Conventional Commits" >&2
    echo "      $subject" >&2
    echo "      expected: <type>(<scope>): <subject>   type one of: $TYPES" >&2
    failures=$((failures + 1))
    continue
  fi

  # Validate the scope against the known module list, when one is present.
  if [[ "$subject" =~ ^[a-z]+\(([a-z0-9/._-]+)\) ]]; then
    scope="${BASH_REMATCH[1]}"
    if [[ ! "$scope" =~ ^($SCOPES)$ ]]; then
      echo "FAIL $short: unknown scope '($scope)'" >&2
      echo "      Scope must be a module path. See CONTRIBUTING.md for the list." >&2
      failures=$((failures + 1))
    fi
  fi

  # Subject line length. The body is where explanation belongs.
  if [[ ${#subject} -gt 72 ]]; then
    echo "FAIL $short: subject is ${#subject} characters, limit is 72" >&2
    failures=$((failures + 1))
  fi

  if [[ "$subject" =~ \.$ ]]; then
    echo "FAIL $short: subject ends with a period" >&2
    failures=$((failures + 1))
  fi

  # Past tense is the commonest slip. Conventional Commits is imperative:
  # "add x", not "added x" / "adds x".
  desc="${subject#*: }"
  first_word="${desc%% *}"
  if [[ "$first_word" =~ (ed|ing)$ ]] && [[ ! "$first_word" =~ ^(add|embed|feed|need|seed|speed|spread|shed|bed|red|deprecated?)$ ]]; then
    echo "WARN $short: '$first_word' may not be imperative mood (use 'add', not 'added'/'adding')" >&2
  fi

  # A breaking change must explain the migration, not just announce itself.
  if [[ "$subject" =~ ^[a-z]+(\([a-z0-9/._-]+\))?!: ]]; then
    if [[ ! "$body" =~ BREAKING\ CHANGE: ]]; then
      echo "FAIL $short: '!' marks a breaking change but there is no 'BREAKING CHANGE:' footer" >&2
      echo "      The footer must state the migration, not just that one is needed." >&2
      failures=$((failures + 1))
    fi
  fi
done < <(git rev-list "$MERGE_BASE"..HEAD)

echo
if [[ $failures -gt 0 ]]; then
  echo "$failures problem(s) across $checked commit(s). See docs/adr/0005-commits-and-changelog.md." >&2
  echo "Rewrite with: git rebase -i $MERGE_BASE" >&2
  exit 1
fi
echo "ok: $checked commit(s) conform to Conventional Commits"
