# The shared-bottleneck lab (docs/73 RU4): tc, a bulk TCP flow and ping, in a
# network namespace of its own so the bottleneck sits on the container's lo.
# glibc, so the host-built lab binaries run as they are.
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 iperf3 iputils-ping \
  && rm -rf /var/lib/apt/lists/*
