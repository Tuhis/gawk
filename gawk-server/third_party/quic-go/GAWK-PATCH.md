# Vendored quic-go v0.62.0 + three gawk patches

> **Modification notice**: the files in this directory are a copy of
> upstream `github.com/quic-go/quic-go` v0.62.0 (MIT, © the quic-go
> authors & Google, Inc — `LICENSE` alongside this file) **modified by the
> gawk authors**. The changed files are `http3/state_tracking_stream.go`,
> `http3/conn.go`, `datagram_queue.go`, `connection.go`, `packet_packer.go`,
> `internal/wire/datagram_frame.go` and `internal/wire/log.go`; each changed
> site carries a `GAWK PATCH` marker.

`gawk-server/go.mod` points at this directory with
`replace github.com/quic-go/quic-go => ./third_party/quic-go`. The
`require` line beside it still names the upstream version this copy is
derived from, and that is the version the attribution notices list.

## Patch 1: datagram receive queues of 512

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

## Patch 2: GSO batching of equal-size packets

On Linux, quic-go hands the kernel a run of packets in one `sendmsg` with
UDP GSO, but upstream only continues a batch after a packet of exactly the
path's maximum size. A DATAGRAM packet almost never is: a full 1200-byte
chunk with http3's session prefix makes a packet of about 1235 bytes,
against a maximum of 1280 to 1452. So every chunk to every viewer left
in a `sendmsg` of its own, and that system call was the largest single
item in the relay's CPU profile (about 27 % with 200 datagram viewers).

`sendPacketsWithGSO` in `connection.go` now takes the batch's segment size
from its first packet when that packet is at least QUIC's minimum of 1200
bytes, packs every later packet with that size as its cap, and continues
while they come out exactly that size. The kernel requires every segment
but the last to be the segment size, and the last may be shorter. Upstream
behaviour is the special case where the first packet is full-size. A
shorter first packet (an ACK, a stream's tail) is sent alone as before, so
it never shrinks the packets behind it.

The cap needs one change in `packet_packer.go`. The packer discards a
DATAGRAM frame that doesn't fit the packet, because upstream's limit is the
largest packet the path takes. Under a batch's cap it could fit a later
packet, so the new `AppendBatchedPacket` leaves it queued, and the batch
schedules another send so it goes out uncapped next.

Measured on loopback with `gawk-relay`, one 60 fps ~10 Mbps
`gawk-pubsim -fixture large -audio` publisher and `gawk-loadgen` viewers,
on a shared 8-core host (2026-10-24). Delivered bytes and frame gaps were
the same with and without the patch.

| Viewers | Without | With |
|---|---|---|
| 25 datagram viewers, 6 runs | 0.97–1.29 cores | 0.61–0.71 cores |
| 25 reliable viewers, 3 runs | 1.10–1.22 cores | 0.90–0.98 cores |
| 50 / 100 datagram viewers (host saturated) | 2.8 / 2.1 cores per delivered Gbps | 2.4 / 1.7 |

Loopback charges the sender for the receiver's kernel work, so on a real
NIC the saving is smaller, though GSO's fewer system calls and socket
buffers are real there too. It applies to `gawk-server` and `gawk-relay`
alike: viewer egress and the origin's `/internal/subscribe` pulls to edges.
A `gawk-relay` on Windows or macOS gains nothing: quic-go has no batched
send on them.

`internal/transport/datagram_gso_test.go` keeps it honest (Linux only, and
skipped where the kernel has no UDP GSO). With stock quic-go, 400 full
chunks leave in 404 writes and none of them batched; with the patch, in
about 30. Its second test sends datagrams of two sizes and fails (about 76
of 400 never arrive) if a datagram that doesn't fit under the cap is
discarded instead of kept.

## Patch 3: one pooled buffer per sent datagram

Upstream allocates every WebTransport datagram it sends twice. http3's
`rawConn.sendDatagram` builds a new buffer to put the session's quarter
stream ID in front of the payload (its own comment: "this creates a lot of
garbage and an additional copy"), and `Conn.SendDatagram` copies that into
the DATAGRAM frame. On the relay that is about 2.5 KB of garbage per
chunk per viewer, which was most of what it allocated and, through that,
most of its GC work.

http3 now passes the prefix and the payload to a new
`Conn.SendDatagramPrefixed` (`connection.go`), which assembles them once in
a buffer from a `sync.Pool` (`datagram_queue.go`). The frame carries the
buffer (`DatagramFrame.Buf`, `internal/wire/datagram_frame.go`), and the
packer returns it to the pool as soon as the frame is written into a packet
(`packet_packer.go`). Nothing reads a DATAGRAM frame's bytes after that:
datagrams are never retransmitted, and the logging that sees the frame
later reads only its length, which the frame keeps. That includes
`wire.LogFrame` (`internal/wire/log.go`). Upstream printed a DATAGRAM
frame there with `%#v`, bytes and all, so with `QUIC_GO_LOG_LEVEL=DEBUG`
it read a buffer another connection could already be refilling. It now
logs the length, as it does for STREAM and CRYPTO frames. A datagram the queue
refuses (a closed connection) goes back at once. One that is dropped
without being sent (too large for the packet) is left to the GC.
`SendDatagram` itself still copies, because its caller keeps the slice.

An intermediate step was measured on the way: letting `Conn` take
http3's buffer without the second copy, but no pool, saved only about
14 % at 50 viewers and 6 % at 25.

Measured as patch 2 was, with patch 2 in both builds, alternating runs
(2026-10-24). Delivered bytes and frame gaps were the same either way.

| Viewers | Without | With |
|---|---|---|
| 50 datagram viewers, 4 runs | 1.21–1.35 cores (mean 1.30) | 1.01–1.03 cores (mean 1.02), −21 % |
| 25 datagram viewers, 4 runs | 0.79–1.04 cores (mean 0.96) | 0.70–0.83 cores (mean 0.78), −18 % |

An earlier prototype of the same change (a callback in place of
`DatagramFrame.Buf`) measured −23 % at 50 viewers and −24 % at 25.

`internal/transport/datagram_alloc_test.go` keeps it honest. It fans 2000
full chunks out to a datagram viewer through the relay and reads the heap
profile under the subscriber's `Session.SendDatagram`. With stock quic-go
that allocates 2592 bytes per chunk, two 1280-byte buffers and the frame;
with the patch, 69–80. The viewer checks every chunk's bytes, so a buffer
reused before its datagram left shows up as corruption. Under `-race`,
`sync.Pool` drops a quarter of what is put back, so the bound is looser
there. Its `TestDebugLogReadsNoPooledDatagram` runs the same fan-out in a
child test binary with `QUIC_GO_LOG_LEVEL=DEBUG` (quic-go reads the level
once, at start). In a `-race` build it fails on a race report if anything
in the debug log reads a returned buffer, as `LogFrame` did.

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
   applies, update it against the new upstream files.
2. `go mod tidy`, `go test -race ./...` (the burst, GSO and allocation tests must pass), and
   `python3 tools/licenses/gen-notices.py` from the repository root.
3. Review `git diff` of this directory: upstream's changes plus the
   `GAWK PATCH` sites, nothing else.

With the version unchanged, `gawk-derive.sh` followed by `git status`
shows no change; that is how to check nobody edited the copy.

## Dropping it

The goal is to delete this directory: once upstream makes the
per-session datagram queue length configurable (or long enough),
batches equal-size packets with GSO and sends a datagram without copying
it twice, set the length from the relay, drop the `replace` and remove
the Renovate rule. Until then, re-derive it on every quic-go bump.
Dropping only patch 1 still means keeping the copy for patches 2 and 3,
and without them the relay's viewer egress costs a `sendmsg` and two
allocations per datagram again.
