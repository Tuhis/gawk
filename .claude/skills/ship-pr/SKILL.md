---
name: ship-pr
description: Open a PR for the current branch and drive it to merge — watch CI and review comments, fix failures and findings as they arrive, request up to 3 `@claude` review rounds on large PRs (>2k changed lines), and squash-merge once everything is green and resolved. Use when asked to "ship", "open and merge", or "land" a branch.
---

# Ship a PR

Take the current branch from "committed" to "merged on `main`" without
handing back in between, unless a stop condition below is hit.

## Repo facts this relies on

Verify them if something behaves unexpectedly, because they can drift:

- **`main` ruleset**: squash merge only, linear history, and **every review
  thread must be resolved** before merging. The required checks are
  `changes`, `app`, `server`, `server-coverage` and `common-ts-lock`, but this
  skill waits for **every** check, not only the required ones.
- **`claude-review`** (`.github/workflows/claude-code-review.yml`) runs
  automatically on `opened`/`synchronize`/`reopened`. It posts inline
  comments for findings, or a `## Code review` summary comment ("No issues
  found…") when the review is clean. **Once that clean summary exists, later
  runs stop without reviewing**: the code-review plugin treats the PR as
  already reviewed, and the check still goes green. Commits pushed after
  the clean summary are therefore unreviewed (see §4).
- **`@claude`** (`.github/workflows/claude.yml`) answers a PR comment that
  mentions it. The comment must come from a writer or `tuhokuutio[bot]`. The
  reply is an issue comment from `claude[bot]` that starts with
  "**Claude finished …**" and links the job.
- CI runs cancel-in-progress per PR ref, so a push kills the previous run.
  Only the run for the **current head SHA** counts.

## 1. Open the PR

1. Make sure the work is committed, and rebase on `origin/main` if it is
   behind. Run the local gates for the components you touched
   (`CONTRIBUTING.md` §Running the gates). A red check found locally saves
   a full CI cycle.
2. Push with `git push -u origin HEAD`.
3. Write a **Conventional Commits** title. The squash makes it the commit
   on `main` that release-please parses, so get the type and scope right
   (`CONTRIBUTING.md`, including the cross-module release-coupling rules).
   The body says what changes and why. End it with the attribution line from
   the session's instructions, if any.
4. Run `gh pr create --base main --title … --body-file …`. If the host
   offers a PR-linking tool, link the PR.
5. Measure the size, leaving out lockfiles and generated notices:

   ```sh
   gh api --paginate repos/{owner}/{repo}/pulls/<N>/files --jq '.[]
     | select(.filename | test("(package-lock\\.json|Cargo\\.lock|go\\.sum|common-ts\\.lock|THIRD-PARTY-NOTICES[^/]*)$") | not)
     | .additions + .deletions' | awk '{s+=$1} END {print s+0}'
   ```

   Use the paginated API. `gh pr view --json files` stops at 100 files and
   undercounts a large PR.

   **Large = more than 2000.** Measure again after big fix pushes, because a
   PR can cross the line.

## 2. Wait, then react

Wait for events. Don't spin:

- If the host has a PR-watch tool (for example `watch_pull_request`), use
  it and end the turn. It wakes you on check, review and comment events.
- Otherwise, run `gh pr checks <N> --watch --interval 60` in the
  background and get notified when it exits. Then read the comments.

Each time you wake, collect **everything new** before changing anything,
then fix it all in one batch, so one push follows one round of findings:

```sh
gh pr checks <N>                                          # check states for the head SHA
gh api --paginate repos/{owner}/{repo}/pulls/<N>/comments   # inline review comments
gh api --paginate repos/{owner}/{repo}/issues/<N>/comments  # summary + @claude replies
gh api graphql -f query='query{repository(owner:"Tuhis",name:"gawk"){pullRequest(number:<N>){
  reviewThreads(first:100){nodes{id isResolved comments(first:20){nodes{author{login} body path line}}}}}}}'
```

### CI failures

1. Read the log with `gh run view <run-id> --log-failed`, or
   `gh run view --job <job-id> --log` for one job.
2. Sort the failure:
   - **Caused by the change**: fix it. Bug fixes are test-first
     (`CLAUDE.md`, `CODE-REVIEW.md`).
   - **Flaky test**: don't simply rerun it. Fix the flake in this PR if it is
     small and clearly scoped. Otherwise rerun once with
     `gh run rerun <run-id> --failed` and note it in the PR for the owner.
   - **Infrastructure** (the runner died, a registry 5xx, a cancelled run
     that wasn't superseded): run `gh run rerun <run-id> --failed`. If it
     fails the same way twice, stop and report.
3. Run the matching local gate before pushing.

### Review comments

These come from `claude[bot]` and from people. Treat every finding as a
claim to verify, not an order:

- **Real bug**: write a failing test, fix it, and commit it on its own with
  a Conventional message that names the finding.
- **Not a bug, or out of scope**: say why, concretely, with code
  references. Don't make a change just to quiet a reviewer.
- **Reply in the thread**, then **resolve it**. The ruleset blocks merging
  while any thread is open:

  ```sh
  gh api repos/{owner}/{repo}/pulls/<N>/comments/<comment-id>/replies -f body='Fixed in <sha>, test-first. …'
  gh api graphql -f query='mutation{resolveReviewThread(input:{threadId:"<thread-id>"}){thread{isResolved}}}'
  ```

  Resolve threads opened by people only when the fix plainly matches what
  they asked for. If there is any doubt, reply and leave the thread open
  for them.

- For findings in an issue comment, such as an `@claude` round's report,
  reply in one PR comment that covers each point with its commit SHA.

Push the batch, then go back to waiting. Until the PR has a clean summary,
each push gets a new `claude-review`, and its findings need the same
treatment.

## 3. `@claude` review rounds (large PRs only)

Skip this section when the PR is 2000 lines or fewer. The automatic
`claude-review` is enough there.

For a large PR, after the automatic review's findings on the current head
are handled and CI is green, request an explicit round with a PR comment:

```
@claude please review again (round K of 3). Since your last review:

- `<sha>`: <what changed and why; say "your finding, test-first" where it applies>
- …

Focus on <the parts most likely to be wrong>.
```

For round 1, ask for a full review of the PR instead of "since your last
review".

- **At most 3 rounds.** A round counts once its job has **succeeded**: count
  the `claude[bot]` replies starting with "**Claude finished …**" that answer
  your `@claude` comments. Don't count your own comments, because a retry
  would count twice. Counting on the PR survives a context reset.
- Wait for the "**Claude finished …**" reply. If its job failed or it never
  posts within ~20 minutes of the run finishing, check the job with
  `gh run view`. A failed run doesn't use up a round, so retry it once by
  posting the same round's comment again.
- Handle the round's findings as in §2, then request the next round.
- **Stop early** when a round reports nothing actionable. Nits you chose not
  to fix, each with a reply saying why, count as nothing actionable.
- If round 3 still has findings, fix them as usual. Don't request a fourth
  round. Say in the final report that the round limit was reached.

## 4. Merge

Merge only when **all** of these hold for the **current head SHA**:

- Every check has finished, each as `success`, `skipped` or `neutral`, with
  none pending. That includes `claude-review`.
- `claude-review` has posted its clean `## Code review` summary. If commits
  were pushed **after** that summary, they are unreviewed. Ask `@claude` to
  review them, naming the commits as in §3, and handle what it finds. On a
  large PR this is the next round. On a small PR it is a one-off review.
- On a large PR, the `@claude` rounds are finished (a clean round, or 3
  rounds).
- No review thread is unresolved, and no person's comment is unanswered.
- The PR title is still a correct Conventional Commit for the final diff.
  Edit it with `gh pr edit --title` if the scope changed.
- The PR is mergeable. If it conflicts, rebase on `origin/main`,
  force-push with `--force-with-lease`, and go back to §2.

Then run:

```sh
gh pr merge <N> --squash --match-head-commit <the head SHA you checked> --subject "<the PR title> (#<N>)"
```

`--match-head-commit` makes GitHub refuse the merge if a commit landed after
your checks, for example from a person or an `@claude` job. If it refuses,
go back to §2 for the new head.

The repo deletes the head branch on merge. Stop any PR watch, then report
the result: the merged SHA, the number of fix commits, the number of review
rounds, and anything left for the owner (a flake rerun, a declined finding).

## Stop and hand back instead of merging when

- A fix would widen the PR's scope: a new concern, a design decision, a
  change to a "Rejected" or locked-in item in `CLAUDE.md`, or a
  `docs/NN` decision.
- A person asks for changes or has an open question you can't settle with
  code.
- The same check fails 3 times after fixes, or an infrastructure failure
  repeats.
- A reviewer's finding and `CLAUDE.md` or a design doc disagree, and you
  can't tell which one is right.

Say exactly what is blocking and leave the PR open.
