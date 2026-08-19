<!--
The PR TITLE must be a Conventional Commit — `type(scope): subject`.

PRs land by squash merge, so this title becomes the commit subject on main,
and it is the string release-please parses. A non-conforming title releases
nothing and leaves the change out of the changelog. Type/scope table:
CONTRIBUTING.md.
-->

## What and why

<!-- The diff says how. Say what changes, and what made it worth changing. -->

## Verification

<!--
What you ran, and what it said. "Tests pass" is not verification; the name of
the gate and its result is. If something could not be run here, say which and
why — that is more useful than silence.
-->

## Checklist

- [ ] Title is a Conventional Commit, with the right type and scope
- [ ] Commits are signed off (`git commit -s`) — [DCO](https://github.com/Tuhis/gawk/blob/main/CONTRIBUTING.md#sign-your-commits-dco)
- [ ] Tests for anything behavioral; for a bug fix, the test was written first and watched to fail
- [ ] Docs updated where the change makes them wrong — the design doc for a decision, `ROADMAP.md` status, `docs/gotchas.md`, and `BUGS.md` entries deleted when fixed
- [ ] `THIRD-PARTY-NOTICES.md` regenerated if dependencies changed (`python3 tools/licenses/gen-notices.py`)
