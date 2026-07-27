#!/usr/bin/env bash
#
# ADR-0005: a feat/fix/perf commit must carry a changie fragment; other commit
# types must not. This runs in CI on pull requests.
#
# The failure message names the exact command to run, per the PRD §8 error
# standard -- a gate that tells you it failed without telling you how to fix it
# just teaches people to re-run it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:-${GITHUB_BASE_REF:-main}}"
FRAGMENT_DIR=".changes/unreleased"

if ! git rev-parse --verify "$BASE_REF" >/dev/null 2>&1; then
  echo "note: base ref '$BASE_REF' not found; skipping changelog check"
  exit 0
fi

MERGE_BASE="$(git merge-base HEAD "$BASE_REF")"

# Commit subjects unique to this branch.
SUBJECTS="$(git log --format=%s "$MERGE_BASE"..HEAD)"
[[ -z "$SUBJECTS" ]] && { echo "note: no commits ahead of $BASE_REF"; exit 0; }

# Does any commit require a fragment?
NEEDS_FRAGMENT=0
while IFS= read -r subject; do
  [[ -z "$subject" ]] && continue
  if [[ "$subject" =~ ^(feat|fix|perf)(\([^\)]*\))?!?: ]]; then
    NEEDS_FRAGMENT=1
    echo "  requires fragment: $subject"
  fi
done <<< "$SUBJECTS"

# Fragments added on this branch.
ADDED="$(git diff --name-only --diff-filter=A "$MERGE_BASE"..HEAD -- "$FRAGMENT_DIR" | grep -c . || true)"

if [[ "$NEEDS_FRAGMENT" -eq 1 && "$ADDED" -eq 0 ]]; then
  cat >&2 <<EOF

FAIL: this branch has a feat/fix/perf commit but adds no changelog fragment.

  Release notes are written when the change is fresh, not reconstructed from
  commit subjects at release time. See docs/adr/0005-commits-and-changelog.md.

  Fix:
      make changelog          # interactive prompt
      git add .changes/unreleased/
      git commit --amend --no-edit

EOF
  exit 1
fi

if [[ "$NEEDS_FRAGMENT" -eq 0 && "$ADDED" -gt 0 ]]; then
  cat >&2 <<EOF

FAIL: this branch adds a changelog fragment but has no feat/fix/perf commit.

  Only user-visible changes belong in the changelog. If this change is
  user-visible, retype the commit. If it is not, remove the fragment.

EOF
  exit 1
fi

echo "ok: changelog fragments consistent with commit types (${ADDED} fragment(s))"
