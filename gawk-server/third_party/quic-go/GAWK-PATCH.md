# Vendored quic-go v0.62.0 + one gawk patch

> **Modification notice**: the files in this directory are a copy of
> upstream `github.com/quic-go/quic-go` v0.62.0 (MIT, © the quic-go
> authors & Google, Inc — `LICENSE` alongside this file) **modified by the
> gawk authors**. The changed files are `http3/state_tracking_stream.go`
> and `datagram_queue.go`; each changed site carries a `GAWK PATCH` marker.

`gawk-server/go.mod` points at this directory with
`replace github.com/quic-go/quic-go => ./third_party/quic-go`. The
`require` line beside it still names the upstream version this copy is
derived from, and that is the version the attribution notices list.

## The change

`streamDatagramQueueLen` in `http3/state_tracking_stream.go` goes from 32
to **512** (`gawk.patch`). It bounds how many received datagrams quic-go's
HTTP/3 layer keeps per WebTransport session until the application reads
them; beyond it, new ones are silently dropped.

Why (`docs/75-reliable-uplink.md` F7, F16, D7, OD4): at the highest rung a
delta is a burst of 52–84 datagrams, and 32 is about a millisecond of them
at a 300 Mbps arrival rate. Any scheduling gap in the relay's reading
goroutine dropped the burst's tail — on a clean path, on a relay with CPU
to spare, after the network had delivered and acknowledged it — and each
lost tail broke the rest of its GOP for every viewer. 512 holds a top-rung
delta about six times over; deeper only turned loss into delay once the
relay was short of CPU (§11 table N). A relay-side reader that drains the
queue faster was measured and does not help.

It matters on two sessions: the origin's publisher sessions and the edge's
`/internal/subscribe` pull sessions, which read origin → edge datagrams
through the same queue. Viewers' sessions send almost nothing, so they
never fill it. Worst case ≈ 0.6 MB per publisher session.

The second site, `maxDatagramRcvQueueLen` in `datagram_queue.go`, goes
from 128 to **512** to match. It is the per-connection queue in front of
the session queue: received datagrams wait there until http3's one
dispatching goroutine (`rawConn.receiveDatagrams`) moves them to their
session. On the lab's network-paced path raising it alone changed nothing
(`docs/75` table G, R-only128), but a burst that arrives faster than that
goroutine is scheduled overflows it before the session's 512 can hold
anything: under CPU contention the burst tests below lost the tail of
their loopback burst there (PR #547's `server-coverage` run; 8 failures
in 300 runs under load at 128, none attributable to it at 512). Its
worst case is the same ≈ 0.6 MB, per connection.

`internal/transport/datagram_burst_test.go` is what keeps it honest: it
fails (33 of 200 datagrams arrive) whenever the relay builds against a
quic-go with the stock length.

## What is in the copy

Every non-test `.go` file of the module, its `LICENSE`, `go.mod` and
`go.sum` — without upstream's `example/`, `interop/`, `integrationtests/`,
`internal/mocks/` and `testdata/` trees, which nothing here builds.
`testutils/` stays: `go mod tidy` resolves the packages that
webtransport-go's tests import, and `testutils/simnet` is one. Nothing in
this directory is edited by hand except `gawk.patch`, this file and
`gawk-derive.sh`.

## Bumping quic-go

Renovate is told to leave quic-go alone (`renovate.json5`): a version
bump of the `require` line alone would change nothing that is built and
only make the line lie. To bump:

1. In `gawk-server/`, `go get github.com/quic-go/quic-go@vX.Y.Z` (and
   webtransport-go if it moves too), then run
   `third_party/quic-go/gawk-derive.sh`. It downloads that version,
   re-copies it here and re-applies `gawk.patch`; if the patch no longer
   applies, update the patch against the new `state_tracking_stream.go`.
2. `go mod tidy`, `go test -race ./...` (the burst tests must pass), and
   `python3 tools/licenses/gen-notices.py` from the repository root.
3. Review `git diff` of this directory: upstream's changes plus the one
   `GAWK PATCH` site, nothing else.

With the version unchanged, `gawk-derive.sh` followed by `git status`
shows no change; that is how to check nobody edited the copy.

## Dropping it

The goal is to delete this directory: once upstream makes the
per-session datagram queue length configurable (or long enough), set it
from the relay, drop the `replace` and remove the Renovate rule. Until
then, re-derive it on every quic-go bump.
