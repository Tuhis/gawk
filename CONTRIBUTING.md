# Contributing to gawk

Thanks for looking. gawk is a side project maintained by one person, so the
honest expectations first: **issues and small, focused pull requests are very
welcome; large ones are best discussed before you write them.** A big PR that
cuts across the relay, the wire format and two broadcasters can sit unreviewed
for a long time, which is nobody's idea of a good outcome. Open an issue and
we can scope it together.

If you are reporting a **security vulnerability**, do not open an issue —
follow [SECURITY.md](SECURITY.md) instead.

## Sign your commits (DCO)

gawk uses the [Developer Certificate of Origin](https://developercertificate.org/)
rather than a CLA. It is one line, and it means you are asserting that you
wrote the contribution, or otherwise have the right to submit it under this
project's license:

```sh
git commit -s -m "fix(relay): ..."
```

`-s` appends `Signed-off-by: Your Name <your@email>`. That is the whole
process — no separate form to sign, no copyright assignment. Your
contributions stay yours, licensed to everyone under
[Apache-2.0](LICENSE) like the rest of the repository.

## Using AI

**A large part of gawk was written with AI assistance** — code, tests and
design docs alike, including much of what you are reading. That is stated
plainly because it is true and because it shapes what this section asks for,
not as a disclaimer. AI-assisted contributions are welcome and expected to
continue; there is no label to apply and no separate process to follow.

There are two requirements, and they are the same two whether or not a model
was involved.

**You understand every line you are submitting.** Not "it passes the tests"
and not "the model explained it to me" — you can say why the code is shaped
the way it is, what happens when it fails, and which of the alternatives it
rejected. If a reviewer asks about a branch you did not know was there, the
PR was not ready. The [DCO sign-off](#sign-your-commits-dco) is where you
assert the right to submit a contribution; this is the standard for having
authored it.

**Scope it and build it as you would have before any of this existed.** The
thing AI changes is not the quality ceiling, it is how cheap volume becomes,
and volume is precisely what a solo-maintained project cannot absorb. So:
the change you would have made by hand, not the one that was free to
generate. No speculative abstraction because it cost nothing to add. No
drive-by rewrite of a file you were passing through. No configuration knob
nobody asked for. One concern per PR, tests for the behaviour that changed,
and a diff someone can hold in their head.

A concrete version of the same rule: if the honest summary of a PR is "I
asked for X and this is what came back", it is not finished — that is a
draft you have not yet reviewed as its author. Read it, cut what does not
belong, and submit the part you would defend.

## Commit messages are load-bearing

Releases are cut by [release-please](https://github.com/googleapis/release-please)
from **[Conventional Commits](https://www.conventionalcommits.org/)**, so the
message you write is what determines the next version number and what appears
in the changelog. Get the type and scope right:

```
feat(r35): single-app sharing (window + app audio) on Linux
fix(relay): drop the subscriber queue instead of blocking the hub
docs(roadmap): propose R36 — telemetry UI usability pass
chore(deps): bump quic-go to v0.61.0
```

`feat` bumps the minor version, `fix` the patch, and `!`/`BREAKING CHANGE:`
the major. Anything else (`docs`, `chore`, `test`, `refactor`, `ci`) releases
nothing. The version and the release notes are the only things this affects —
it changes no behavior, which is exactly why it is easy to get wrong and worth
checking before you push.

## Where things live

`CLAUDE.md` has the map, and it is a rule rather than a courtesy: each fact
has exactly one authoritative home, and duplicating it elsewhere is how the
docs drifted last time. In short:

| You want | Read |
|---|---|
| What's built and what's next | [`ROADMAP.md`](ROADMAP.md) |
| Why a milestone is designed the way it is | its `docs/NN-*.md` |
| Known bugs | [`BUGS.md`](BUGS.md) |
| Coding + review rules, and the gates | [`CODE-REVIEW.md`](CODE-REVIEW.md) |
| Building/running one component | that component's `README.md` |

Two rules from those documents are worth repeating here, because a PR that
misses them will be sent back:

**Bug fixes are test-first.** Write the failing test, watch it fail, then fix
it. Every rule in `CODE-REVIEW.md` exists because its absence cost a real bug;
this one most of all.

**The wire format has four mirrors.** `gawk-server/wire/wire.go` is the source
of truth. `gawk-app`'s `wire.ts`, `gawk-broadcast/internal/wirecheck` and
`gawk-broadcast-windows/crates/wire` each restate it, with golden vectors kept
**byte-identical** across all four — deliberately restated, never imported or
generated, because a shared fixture could be edited once and stay green
everywhere. A new wire type or close code lands in all four in one PR, or it
does not land.

## Running the gates before you push

CI runs these on every PR; running them locally first is much faster than
waiting for a red check.

```sh
cd gawk-server            && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-broadcast         && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-telemetry         && go vet ./... && go test ./...   # plus -tags duckdb
cd gawk-app               && npm ci && npm run lint && npm test && npm run build
cd gawk-telemetry/ui      && npm ci && npm run lint && npm test && npm run build
cd gawk-broadcast-windows && cargo test --workspace && \
                             cargo xwin clippy --all-targets --target x86_64-pc-windows-msvc -- -D warnings
```

Every Go module must also be `go mod tidy`-clean — the `tidy` job checks all
three independently, because a bump in one module can leave another (which
reaches it through a local `replace`) with a stale graph.

The native Windows broadcaster cross-compiles from Linux with
[cargo-xwin](https://github.com/rust-cross/cargo-xwin); see
[`gawk-broadcast-windows/README.md`](gawk-broadcast-windows/README.md) before
touching that build.

## Adding a dependency

Dependencies are held to gawk's license policy, and CI enforces it: the
`licenses` job fails on anything outside the permissive, notice-only set
(`tools/licenses/gen-notices.py` for Go and npm, `deny.toml` for Rust).
Copyleft dependencies — GPL, LGPL, AGPL — cannot be linked into what this
project redistributes. If you believe an exception is warranted, raise it in
the issue *before* writing the code; it is a licensing decision, not a build
fix.

When a dependency does change, regenerate the attribution files:

```sh
python3 tools/licenses/gen-notices.py
```

and commit the resulting `THIRD-PARTY-NOTICES.md` files. The `licenses-fresh`
CI job regenerates them itself and fails if your commit does not match, so
this is not something you can forget — but it is quicker to run it than to
learn about it from a red check. It needs Go, Node, Python and cargo on your
PATH, and it goes to the network.

For **Renovate PRs this happens automatically**: `postUpgradeTasks` in
`renovate.json5` runs the generator on the bump branch, so the notices land in
the same PR as the version change. That block only takes effect when the
self-hosted bot's global config allowlists the command; if Renovate PRs start
failing `licenses-fresh`, that allowlist is what is missing.

## What a good PR looks like

- One concern per PR. "Fix the thing" plus "and reformat this file" is two.
- Tests for anything behavioral, written before the fix if it is a bug.
- The design doc updated when a decision changes, not just the code.
- `ROADMAP.md` status and `docs/gotchas.md` kept in sync when your change
  makes them wrong. `BUGS.md` entries deleted when your change fixes them.
- A description that says what changes and why; the diff already says how.
