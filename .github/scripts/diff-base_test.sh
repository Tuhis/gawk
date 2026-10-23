#!/bin/sh
# Tests for .github/scripts/diff-base.sh, the base CI's "What moved?" filter
# and the migration-immutability gate diff against. Each case builds the
# commit graph a GitHub event checks out, in a throwaway repository: no
# network, nothing touched in the real checkout. Run from the repo root:
#
#     ./.github/scripts/diff-base_test.sh
set -eu

SCRIPT=$(cd "$(dirname "$0")" && pwd)/diff-base.sh
PASS=0
FAIL=0
ZERO=0000000000000000000000000000000000000000

ok()  { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
cd "$T"
git init -q -b main .
git config user.email ci@example.invalid
git config user.name ci
commit() { # commit <path> → the new commit's SHA
    mkdir -p "$(dirname "$1")"
    echo "$1 $(date +%s%N)" >> "$1"
    git add "$1"
    git commit -q -m "$1"
    git rev-parse HEAD
}

# main: A. The PR branches from A and changes docs/x.md. main then moves on
# (B touches gawk-app/), and the PR's merge ref is built on B, as GitHub
# builds refs/pull/N/merge against the base branch's tip: first parent B,
# second parent the PR head.
A=$(commit README.md)
git switch -q -c pr
HEAD_SHA=$(commit docs/x.md)
git switch -q main
B=$(commit gawk-app/README.md)
git switch -q --detach "$B"
git merge -q --no-ff -m merge pr
MERGE=$(git rev-parse HEAD)

# expect_moved <description> <expected changed files> <args…>
expect_moved() {
    desc=$1 want=$2
    shift 2
    base=$("$SCRIPT" "$@")
    if [ -z "$base" ]; then
        bad "$desc (no base printed)"
        return
    fi
    got=$(git diff --name-only "${base}...HEAD" | tr '\n' ' ' | sed 's/ $//')
    if [ "$got" = "$want" ]; then ok "$desc"; else bad "$desc (moved: '$got', want '$want')"; fi
}
expect_base() { # expect_base <description> <expected base, or empty> <args…>
    desc=$1 want=$2
    shift 2
    got=$("$SCRIPT" "$@")
    if [ "$got" = "$want" ]; then ok "$desc"; else bad "$desc (base: '$got', want '$want')"; fi
}

echo "pull_request, checked out at the merge ref:"
expect_moved "base.sha older than the merge's base: only the PR's own files moved" \
    "docs/x.md" pull_request "$A" "$HEAD_SHA" ""
expect_moved "base.sha current: only the PR's own files moved" \
    "docs/x.md" pull_request "$B" "$HEAD_SHA" ""

echo "pull_request, checked out at the PR head:"
git switch -q --detach "$HEAD_SHA"
expect_base "not a merge: base.sha is the base" "$A" pull_request "$A" "$HEAD_SHA" ""
expect_base "unknown base.sha: no base" "" pull_request "$ZERO" "$HEAD_SHA" ""

echo "pull_request, a head that is itself a merge of main:"
git switch -q pr
git merge -q --no-ff -m "merge main into pr" main
OWN_MERGE=$(git rev-parse HEAD)
git switch -q --detach "$OWN_MERGE"
expect_base "the head's own merge is not the merge ref: base.sha is the base" \
    "$B" pull_request "$B" "$OWN_MERGE" ""

echo "push and others:"
git switch -q --detach "$MERGE"
expect_base "push: before is the base" "$B" push "" "" "$B"
expect_base "push: a new branch has no base" "" push "" "" "$ZERO"
expect_base "push: a before that is not here has no base" "" push "" "" "$(printf 'f%.0s' $(seq 40))"
expect_base "workflow_dispatch: no base" "" workflow_dispatch "" "" ""

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
