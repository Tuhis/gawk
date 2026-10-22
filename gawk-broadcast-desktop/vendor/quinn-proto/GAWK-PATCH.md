# Vendored quinn-proto 0.11.19 + one gawk patch

> **Modification notice** (Apache-2.0 §4(b)): the files in this directory are
> a copy of upstream `quinn-proto` 0.11.19 as published on crates.io (MIT OR
> Apache-2.0, © the quinn developers; `LICENSE-MIT` and `LICENSE-APACHE`
> alongside this file) **modified by the gawk authors**. The changed files
> are `src/congestion/bbr/bw_estimation.rs` and `src/congestion.rs`; each
> changed site carries a `GAWK PATCH` marker.

The workspace's `[patch.crates-io]` points at this directory, and so do the
`[patch]` tables of `gawk-ios/rust` and `tools/uplinklab`: a `[patch]` only
applies inside the workspace that declares it. The version the copy was
derived from is the one `Cargo.lock` lists, and the one the attribution
notices list.

## The change

BBR's bandwidth estimate is a windowed max filter over the last 10 rounds.
The filter only ages old samples out when it is fed new ones, and upstream's
`BandwidthEstimation::on_ack` fed it only samples above the current maximum.
So the estimate could never fall. A single high sample stayed the
connection's bandwidth for the rest of its life, and BBR paced and sized its
window for it.

High samples are easy to get. quinn sends a batch of packets with one `now`
(`drive_transmit`), which makes the estimator's send rate `u64::MAX`, so a
sample is just the ack rate. Two acks a few microseconds apart (ack
compression: Wi-Fi aggregation, a receiver or network that bunches acks)
are a sample of hundreds of MB/s. The tests below take one pair of acks
10 µs apart from 1.2 MB/s to 120 MB/s. Before the patch it was still
120 MB/s 200 rounds later; with it, the estimate is back to 1.2 MB/s once
the spike leaves the window. A path that genuinely slows down was never
seen either: the estimate stayed at the old rate.

The fix is in `bw_estimation.rs`: every non-zero sample goes to the filter,
which keeps its max-over-10-rounds behaviour. A zero is the second ack
processed at the same `now`, not a measurement. This does not stop a spike
from entering the window; that needs per-packet delivery-rate sampling
(what BBR's specification does) and is upstream work.

The engine's measured results for BBR (`docs/75` F17, "quinn's BBR overruns
an uplink the stream nearly fills") were taken before this patch.

`src/congestion.rs` adds `GAWK_BBR_MAX_FILTER_PATCH`, a hidden constant
that `gawk-engine` names (`crates/engine/src/transport.rs`). Cargo skips a
`[patch]` whose version no longer matches the requirement with only a
warning; the constant turns that, or a workspace without the `[patch]`,
into a compile error.

The regression tests are in the patch, at the end of `bw_estimation.rs`
(`gawk_tests`). They are not part of `cargo test --workspace` (vendor/ is
excluded from the workspace); the desktop workflow's `test` job runs them:

```sh
cargo test --manifest-path vendor/quinn-proto/Cargo.toml --no-default-features --lib congestion
```

## What is in the copy

The published `.crate`, minus its `Cargo.lock` and `.cargo-ok`, plus
`gawk.patch`. Nothing here is edited by hand except `gawk.patch`, this file
and `gawk-derive.sh`.

## Bumping quinn-proto

A plain `cargo update -p quinn-proto` changes nothing that is built: the
lock entry follows this directory's version. A quinn bump that needs a
newer quinn-proto than this copy makes Cargo skip the `[patch]`, and the
engine then fails to compile on `GAWK_BBR_MAX_FILTER_PATCH`. Either way,
to bump:

1. Check whether upstream fixed it (`bw_estimation.rs`: does `on_ack` still
   call `update_max` only when `max_filter.get() < bandwidth`?). If it did,
   delete this directory, the three `[patch]` lines and the engine's
   `const _` check, and stop here.
2. Set the version: in `gawk-broadcast-desktop/Cargo.lock`, change the
   `quinn-proto` entry's `version` to the new one (the entry has no
   `source` while the patch is in use), then run
   `vendor/quinn-proto/gawk-derive.sh`. It downloads that version, checks
   it against the crates.io index checksum, re-copies it here and
   re-applies `gawk.patch`. If the patch no longer applies, port it to the
   new `bw_estimation.rs` and regenerate `gawk.patch` with
   `diff -ruN a b` against a pristine copy (strip the timestamps).
3. `cargo update -p quinn --precise <new>` if quinn moves too, then
   `cargo check` in this workspace, `gawk-ios/rust` and
   `tools/uplinklab` (each has its own `[patch]`).
4. Run the regression tests above, `cargo test --workspace`, and
   `python3 tools/licenses/gen-notices.py` from the repository root.
5. Review `git diff` of this directory: upstream's changes plus the
   `GAWK PATCH` sites, nothing else.

With the version unchanged, `gawk-derive.sh` followed by `git status`
shows no change; CI checks exactly that.
