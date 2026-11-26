# gawk-mmproxy image (R88, docs/92). It runs only inside the operator's
# murmur pod: an init container (`routes`) and a sidecar (`serve`). There is
# no chart (docs/92 OD8); deploy/k8s and deploy/compose hold the snippets.
#
# The build context is the REPO ROOT, for LICENSE beside the module, as for
# every gawk image:
#
#   docker build -f gawk-mmproxy/deploy/Dockerfile -t gawk-mmproxy:dev .
#   docker build -f gawk-mmproxy/deploy/Dockerfile --target test .   # gofmt, vet, the suite
#
# Pure Go (CGO_ENABLED=0), standard library and golang.org/x/sys only.
#
# Privilege (docs/92 D5, F11, F14). The binary carries the file capability
# cap_net_raw+ep, so `serve` runs as the distroless nonroot user (65532)
# with only NET_RAW in its bounding set, which IP_TRANSPARENT needs and
# nothing more. Because of that file capability, EVERY container that execs
# this image must have NET_RAW in its bounding set, or the exec itself fails
# with EPERM ("exec /gawk-mmproxy: operation not permitted", measured
# 2026-11-26): the `routes` init container adds NET_RAW beside NET_ADMIN and
# runs as uid 0.

FROM golang:1.27-trixie AS build
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends libcap2-bin >/dev/null && rm -rf /var/lib/apt/lists/*
WORKDIR /src/gawk-mmproxy
COPY gawk-mmproxy/go.mod gawk-mmproxy/go.sum ./
RUN go mod download
COPY gawk-mmproxy/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-mmproxy ./cmd/gawk-mmproxy \
 && setcap cap_net_raw+ep /out/gawk-mmproxy \
 && getcap /out/gawk-mmproxy | grep -q cap_net_raw

# `docker build --target test`: the suite. A build step can't create user
# namespaces, so the namespaced tests skip here; CI runs them in a container
# that can (the mmproxy job). GAWK_MMPROXY_ISOLATED: the bridge's copy of the
# PROXY v2 vectors isn't in this build context.
FROM build AS test
ENV GAWK_MMPROXY_ISOLATED=1
RUN test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# ── runtime ───────────────────────────────────────────────────────────────
# Last in the file, so a plain `docker build` (the release publish) builds
# it. Distroless, nonroot (uid 65532), no shell. COPY keeps the file
# capability (BuildKit preserves security.capability).
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gawk-mmproxy /gawk-mmproxy
COPY LICENSE /LICENSE
COPY gawk-mmproxy/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
# 64739/tcp: PROXY v2 from gawk-mumble-bridge pods only (a ClusterIP
#            Service and a NetworkPolicy, docs/92 D9).
# 2114/tcp:  /healthz, /readyz, /metrics. Internal only.
EXPOSE 64739/tcp 2114/tcp
ENTRYPOINT ["/gawk-mmproxy"]
CMD ["serve"]
