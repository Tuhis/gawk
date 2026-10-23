#!/bin/sh
# Prints the commit a CI run diffs HEAD against to see what the change moved,
# or nothing when there is no usable base (the caller then assumes everything
# moved). Run from the checkout, after a full-depth fetch:
#
#     diff-base.sh <event_name> <pull_request.base.sha> <pull_request.head.sha> <push before>
set -eu

event=$1 pr_base=$2 pr_head=$3 before=$4

case "$event" in
    pull_request)
        # actions/checkout checks out the PR's merge ref, which GitHub builds
        # against the base branch's tip when the run starts: first parent that
        # tip, second parent the PR head. The event's base.sha can name an
        # older base commit (#544's run on 7e88d23 had base.sha 22670b81 and a
        # merge built on 66d2eebb, four main commits later), and a diff from
        # it counts the base branch's own new commits as the PR's: a docs PR
        # ran the app, e2e and dev-stack jobs for a README fix on main. The
        # merge's first parent is the base it was built on. Anything else
        # checked out (the PR head, or a head that merged main itself) keeps
        # base.sha.
        if [ -n "$pr_head" ] \
            && [ "$(git rev-parse -q --verify 'HEAD^2' 2>/dev/null || true)" = "$pr_head" ]; then
            base=$(git rev-parse 'HEAD^1')
        else
            base=$pr_base
        fi
        ;;
    push)         base=$before ;;
    *)            base= ;;
esac

# No usable base: a first push on a branch, a force-push, a workflow_dispatch.
if [ -z "$base" ] \
    || [ "$base" = "0000000000000000000000000000000000000000" ] \
    || ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
    exit 0
fi
echo "$base"
