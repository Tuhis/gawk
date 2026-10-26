---
name: ship-pr
description: Open a PR for the current branch and drive it to merge — watch CI and review comments, fix failures and findings as they arrive, request up to 3 `@claude` review rounds (always on large PRs over 2k changed lines, and on any PR to review fixes the automatic review skips), and squash-merge once everything is green and resolved. Use when asked to "ship", "open and merge", or "land" a branch.
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
  automatically on `opened`/`synchronize`/`reopened`, but it **reviews a PR
  once**. It posts inline comments for findings, or a `## Code review`
  summary ("No issues found…") when the review is clean. Every later run
  stops as soon as it sees that Claude has already commented on the PR,
  and the check still goes green. Commits pushed after the first review,
  including the fixes for its findings, are therefore **not reviewed
  automatically** (see §3 and §4).
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
  reviewThreads(first:100){pageInfo{hasNextPage} nodes{id isResolved comments(first:20){nodes{author{login} body path line}}}}}}}'
```

If `hasNextPage` is true, a thread may be missing from the list. Stop and
report rather than merge on a partial list.

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

Push the batch, then go back to waiting. The automatic review won't look at
the fixes. §3 gets them reviewed.

## 3. `@claude` review rounds

A round is requested in two cases. Both share one cap of 3 rounds per PR:

- **Large PR (more than 2000 lines):** always request rounds, even when the
  automatic review was clean.
- **Any PR with commits pushed after the automatic review**, such as fixes
  for its findings: request rounds until those commits are reviewed. A small
  PR whose automatic review was clean and that got no later commits needs
  no rounds.

**Track which commit each review covered.**

- **Automatic review with findings:** the `original_commit_id` of its
  inline comments in `pulls/<N>/comments`.
- **Automatic review with a clean summary:** the `headSha` of the
  `claude-review` run whose `createdAt`…`updatedAt` window contains the
  summary's `created_at` (`gh run list --workflow claude-code-review.yml
  --branch <branch> --json headSha,createdAt,updatedAt`). Both times come
  from GitHub, so this is safe.
- **No Claude review comment on the PR at all:** nothing is reviewed. The
  check can go green without posting anything. Round 1 is then a full
  review of the PR.
- **A round:** the head SHA when you posted the request, so name it in the
  comment.

 The commits still unreviewed
are `git log --oneline <last reviewed sha>..HEAD`. Don't compare commit
dates with comment times, because a commit can be made before a review and
pushed after it.

Request a round once the previous review's findings are handled and CI is
green on the current head. Post a PR comment:

```
@claude please review again (round K of 3), up to `<head sha>`. Since your last review (`<last reviewed sha>`):

- `<sha>`: <what changed and why; say "your finding, test-first" where it applies>
- …

Focus on <the parts most likely to be wrong>.
```

On a large PR with a clean automatic review, ask for a full review of the
PR in round 1, instead of "since your last review".

- **At most 3 rounds.** A round counts once its job has **succeeded**: count
  the `claude[bot]` replies starting with "**Claude finished …**" that answer
  your `@claude` comments. The reply must actually review the requested
  range. One that says it couldn't (a failed checkout, out of turns) is a
  failed run. It is neither a round nor clean. Don't count your own comments, because a retry
  would count twice. Counting on the PR survives a context reset.
- Wait for the "**Claude finished …**" reply. If its job failed or it never
  posts within ~20 minutes of the run finishing, check the job with
  `gh run view`. A failed run doesn't use up a round, so retry it once by
  posting the same round's comment again.
- Handle the round's findings as in §2, then request the next round.
- **Stop early** when a round leaves nothing to push. That means it reports
  nothing actionable, or you declined each finding with a reply saying why.
- If round 3 still has findings, fix them as usual. Don't request a fourth
  round. Say in the final report that the round limit was reached.

## 4. Merge

Merge only when **all** of these hold for the **current head SHA**:

- Every check has finished, each as `success`, `skipped` or `neutral`, with
  none pending. That includes `claude-review`.
- Every commit has been reviewed. `git log <last reviewed sha>..HEAD`
  (§3) is empty, and every finding of every review is handled. A finding is
  handled when it was declined with a reply, or fixed in a commit that a
  later review covered. A review whose findings were all declined, with
  nothing pushed, satisfies this. Nothing re-runs, and nothing needs to. The only
  exception is a reached 3-round cap: the commits after round 3 then merge
  unreviewed, and the final report names them.
- On a large PR, the `@claude` rounds are finished: a clean round, or 3
  rounds with round 3's findings handled as in §2.
- No review thread is unresolved, and no person's comment is unanswered.
  Every `@claude` report has a reply that covers each of its points.
- The PR title is still a correct Conventional Commit for the final diff.
  Edit it with `gh pr edit --title` if the scope changed.
- The PR is mergeable. If it conflicts, rebase on `origin/main`,
  force-push with `--force-with-lease`, and go back to §2. The reviewed SHA
  is no longer an ancestor of `HEAD`, so compare the two versions with
  `git range-diff <old merge-base>..<last reviewed sha> origin/main..HEAD`.
  Commits that come out identical stay reviewed, and the marker moves to the
  rebased copy of the reviewed SHA. Changed commits, such as conflict
  resolutions, are unreviewed. If the round cap is already used up and the
  rebase changed reviewed code, stop and hand back.

Then run:

```sh
gh pr merge <N> --squash --match-head-commit <the head SHA you checked> --subject "<the PR title> (#<N>)"
```

`--match-head-commit` makes GitHub refuse the merge if a commit landed after
your checks, for example from a person or an `@claude` job. If it refuses,
go back to §2 for the new head.

The repo deletes the head branch on merge. Stop any PR watch, then report the result:
the merged SHA, the number of fix commits, the number of review rounds,
whether the round cap was reached and which commits merged unreviewed, and
anything left for the owner (a flake rerun, a declined finding).

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
