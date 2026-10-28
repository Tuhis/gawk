# gawk-sampler — automatic content screening

Looks at every live broadcast's latest keyframe every few seconds, scores it
with a nudity detector, and — when an explicit signal persists — posts a
**content flag** to `gawk-admin`, which pages the operator and pins the
broadcast in the portal. R40; the design, the decisions and the rejected
alternatives are in [`docs/71`](../docs/71-content-flagging.md) — this file is
only how to build, test and run it.

It is **optional and off by default**: nothing else in the stack requires it.
Installed, it starts in **`observe` mode**, which pages no one — it scores
every broadcast and stores what it *would* have flagged, so an operator sees
their own false-trip rate before anything pages (docs/71 D19).

Three rules travel with it, and the code is shaped around them:

- **No frame ever leaves memory.** Frames live between fetch and score and are
  never written to disk, logged, posted or exposed (D8). ffmpeg runs from pipes
  with `RLIMIT_CORE` 0, so not even a crash writes one.
- **It flags; it never acts.** It holds the `flagger` role and the relay's
  screening token — never the relay admin token (which reads publisher IPs)
  and never `operator`. Every kill is `gawk-admin`'s (D2, D14).
- **No NudeNet code or weights are in this module or its image.** Both are
  AGPL-3.0 (docs/71 §8.1): the pre- and post-processing are reimplemented from
  the YOLOv8 output format, and the weights are fetched at deploy time against
  a pinned SHA-256. Tests and the dev stack run a generated test model.

## How a sample happens

1. **Targets**: every `-targets-refresh` it reads its shard's list from
   `gawk-admin` (`GET /api/v1/screening/targets?shard=<i>&of=<n>`). Pod
   `gawk-sampler-<i>` takes share `i` of `n`; the split is
   `gawk-server/screening.Shard` (D22).
2. **Fetch**: `GET /internal/screening/broadcasts/{id}/keyframe` on the relay
   pod holding the broadcast, with `If-None-Match`, so an unchanged frame costs
   a `304` (D1).
3. **Decode**: the frame's bitstream (AVCC → Annex B, Annex B as is, VP8/VP9 in
   IVF) is piped into an **ffmpeg subprocess**, which hands back one RGB frame
   letterboxed to the model's input (D4, §9.5). A malformed bitstream kills
   one sample, never the sampler.
4. **Score**: ONNX Runtime, in process; a sample's score is the highest
   confidence among the flagged labels (D7).
5. **Window**: `-window-hits` of the last `-window` samples at or above
   `-threshold` trips the window; all of them below clears it. While tripped,
   every sample POSTs `/api/v1/content-flags`; the clear POSTs once (D9).
   Samples above `-sample-floor` are batched to `gawk-admin` every 5 s for
   calibration (D20).

## Build

The module `replace`s `gawk-server` with the local tree, like `gawk-admin` and
`gawk-telemetry`: the screening contract is the public
`gawk-server/screening` package (D3), never a mirror. **The image therefore
builds from the repository root.**

```sh
docker build -f gawk-sampler/deploy/Dockerfile -t gawk-sampler:dev .
```

It is a **cgo** build (`onnxruntime_go`), and it needs two native pieces at
run time, both of which the image carries:

| | What | In the image |
|---|---|---|
| ffmpeg | A minimal build: the h264, vp8 and vp9 decoders and the pipe to one RGB frame, nothing else. LGPL-2.1-or-later, run as a subprocess, never linked. | `/usr/bin/ffmpeg`; licence text, source URL and configure line in `/licenses/ffmpeg` |
| ONNX Runtime | The shared library `onnxruntime_go` loads, at the C API version its headers are written against (`deploy/Dockerfile` pins both). | `/usr/lib/libonnxruntime.so`; licence and notices in `/licenses/onnxruntime` |

To build the same ffmpeg on your machine (a C compiler, `make`, `curl` and
`xz`; `nasm` optional, for the SIMD paths):

```sh
sh deploy/build-ffmpeg.sh "$HOME/.local/gawk-ffmpeg"
```

ONNX Runtime is the `onnxruntime-linux-x64-<version>.tgz` release asset, at the
version and SHA-256 in `deploy/Dockerfile`; its `lib/libonnxruntime.so.<version>`
is the file.

## Test

```sh
cd gawk-sampler
go vet ./... && CGO_ENABLED=1 go test ./...
```

The tests that need the real ffmpeg or ONNX Runtime — bitstream conversion
through ffmpeg, the decode timeout and the subprocess limits, inference on the
test models — **skip** unless they are pointed at the binaries:

```sh
GAWK_SAMPLER_FFMPEG=$HOME/.local/gawk-ffmpeg/bin/ffmpeg \
GAWK_SAMPLER_ONNXRUNTIME_LIB=/path/to/libonnxruntime.so.1.31.0 \
CGO_ENABLED=1 go test ./...
```

`GAWK_SAMPLER_REQUIRE_NATIVE=1` turns that skip into a failure. CI runs the
whole suite that way, inside the image's own build (`docker build --target
test`), against the same ffmpeg and library the runtime image ships, and then
starts the built image: the `sampler` job in `.github/workflows/ci.yml`.

`testdata/` holds what the tests run on, each with the script that regenerates
it beside it: the generated test models (`models/`, a 320 and a 640 model with
NudeNet's input and output shapes that score magenta), one keyframe per codec
(`bitstreams/`), and the post-processing reference vectors (`reference/`,
numbers derived from the real model on public-domain photographs, whose sources
are listed there; neither the model nor the photographs are committed).

## Run

In a cluster, install the chart (below). Every flag is also an environment
variable, `GAWK_SAMPLER_<FLAG>` (`-window-hits` → `GAWK_SAMPLER_WINDOW_HITS`),
and the full table with defaults is [`docs/71` §5](../docs/71-content-flagging.md#5-gawk-sampler-knobs).
The ones a run cannot start without:

| Flag | |
|---|---|
| `-admin-url` | `gawk-admin`'s base URL. |
| `-oidc-issuer`, `-oidc-client-id` | The client-credentials identity; the secret comes from `GAWK_SAMPLER_OIDC_CLIENT_SECRET`. |
| `-screening-token` | The relay's `-screening-token`. |
| `-model`, `-model-input` | The detector file and its square input size (320 or 640). |
| `-shard-index`, `-shard-count` | This pod's share. The index defaults to the ordinal of `$POD_NAME`. |

Subcommands: `run` (the default), `score <dir>` (the scoring path over a local
image directory, printing per-label recall at candidate thresholds — the
offline recall check of D19), `fetch-model -url -sha256 -out` (the chart's
init container: writes the file only when its SHA-256 matches the pin) and
`version`.

`-metrics-addr` (`:9090`) serves `/metrics`, `/healthz` and `/readyz`, and is
never routed: operational numbers only, never a frame or a broadcast ID.

## Deploy

`deploy/charts/gawk-sampler`, published as
`oci://ghcr.io/tuhis/charts/gawk-sampler` with the image
`ghcr.io/tuhis/gawk-sampler` — chart version, appVersion and image tag always
equal. A StatefulSet of `replicas` (2) pods, each screening a fixed share;
the model is fetched by an init container running this image's `fetch-model`
against the pinned NudeNet release asset, or mounted from a volume you
provide. The values file documents every knob; enabling screening end to end
(the relay's `screening.token` and `screening.notice`, `gawk-admin`'s
`screening.enabled`, the `flagger` client) is
[`docs/self-hosting.md`](../docs/self-hosting.md).
