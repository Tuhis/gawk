#!/usr/bin/env bash
# R79 MV7 (docs/80 §9): the one-pod load measurement. One relay, ONE
# gawk-mumble-bridge pod and murmur 1.6 on this machine; gawk-voiceload puts
# N voice participants into one room (each a real room session, ticket and
# voice session, each a real bridged Mumble user) and lets three of them talk
# at once for 30 s. For each N it records frame delivery (what murmur should
# have fanned out vs what the participants received), join latency, and the
# CPU the bridge process and the murmur container used while the talkers
# talked. The results go to docs/assets/80/load/ as one JSON line per run.
#
#   mkdir -p e2e/bin
#   (cd gawk-server && go build -o ../e2e/bin/ ./cmd/gawk-server ./cmd/gawk-devcert)
#   (cd gawk-mumble-bridge && go build -o ../e2e/bin/ ./cmd/gawk-mumble-bridge ./cmd/gawk-voiceload)
#   e2e/voice-load.sh [N ...]          # default: 10 25 50 100
#
# A measurement, not a gate: CI does not run it. The numbers depend on the
# machine, which the output names.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)
bin="$here/bin"
out="$here/out/voice-load"
dest="$repo/docs/assets/80/load"
counts=("${@:-10 25 50 100}")
# shellcheck disable=SC2206
counts=(${counts[*]})
talkers=3
duration=30s

rm -rf "$out"; mkdir -p "$out" "$dest"
export GAWK_MURMUR_PREFIX=gawk-load-murmur GAWK_MURMUR_BASE_PORT=64770
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  "$repo/gawk-mumble-bridge/testdata/murmur.sh" down >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$repo/gawk-mumble-bridge/testdata/murmur.sh" up 1.6 > "$out/murmur.env"
murmur=127.0.0.1:64770
"$bin/gawk-devcert" -out "$out/certs" -hosts localhost,127.0.0.1 >/dev/null
cert="$out/certs/cert.pem" key="$out/certs/key.pem"
echo '[{"code":"voiceload","mumble":{}}]' > "$out/rooms.json"
psk=load-psk-0123456789 secret=load-ticket-secret-0123456789

"$bin/gawk-server" -addr 127.0.0.1:4753 -metrics-addr 127.0.0.1:2752 -cert-file "$cert" -key-file "$key" \
  -rooms -rooms-file "$out/rooms.json" -max-room-participants 250 \
  -voice-bridge-addr 127.0.0.1:4764 -voice-bridge-public-url https://127.0.0.1:4763/voice \
  -voice-bridge-psk "$psk" -voice-ticket-secret "$secret" -voice-bridge-ca-file "$cert" \
  -voice-max-users-per-room 250 -conn-rate-limit 1000 -conn-burst-limit 2000 \
  > "$out/relay.log" 2>&1 &
pids+=($!)
"$bin/gawk-mumble-bridge" -listen 127.0.0.1:4763 -internal-listen 127.0.0.1:4764 -ops-listen 127.0.0.1:2763 \
  -cert-file "$cert" -key-file "$key" -allowed-origins http://127.0.0.1:8080 -psk "$psk" -ticket-secret "$secret" \
  -relay-url https://127.0.0.1:4753 -relay-ca-file "$cert" -mumble-addr "$murmur" -mumble-insecure-skip-verify \
  -max-voice-sessions 500 -log-level warn \
  > "$out/bridge.log" 2>&1 &
bridge=$!
pids+=("$bridge")
for _ in $(seq 1 50); do curl -fsS http://127.0.0.1:2763/readyz >/dev/null 2>&1 && break; sleep 0.2; done
for _ in $(seq 1 50); do curl -fsS http://127.0.0.1:2752/statusz 2>/dev/null | grep -q '"voiceObserver":"available"' && break; sleep 0.2; done

ticks=$(getconf CLK_TCK)
cpu_ticks() { awk '{print $14 + $15}' "/proc/$1/stat"; }
# The bridge's own view of the same frames: what it forwarded to browsers and
# what it dropped, by reason, so a shortfall can be placed (murmur → bridge,
# inside the bridge, or bridge → client).
bridge_frames() {
  curl -fsS http://127.0.0.1:2763/metrics | awk '
    /^gawk_bridge_frames_forwarded_total\{direction="to_browser"\}/ {f=$2}
    /^gawk_bridge_frames_dropped_total/ {d+=$2}
    END {printf "%d %d\n", f, d}'
}
murmur_cpu() { docker stats --no-stream --format '{{.CPUPerc}}' gawk-load-murmur-main | tr -d '%'; }
machine="$(nproc) cores, $(awk -F: '/model name/{print $2; exit}' /proc/cpuinfo | sed 's/^ *//')"
stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)

for n in "${counts[@]}"; do
  echo "== $n users, $talkers talking" >&2
  "$bin/gawk-voiceload" -relay https://127.0.0.1:4753 -room voiceload -origin http://127.0.0.1:8080 \
    -ca-file "$cert" -users "$n" -talkers "$talkers" -duration "$duration" \
    -fixture "$repo/gawk-mumble-bridge/testdata/tone-440hz-10ms.opus" > "$out/load-$n.json" &
  load=$!
  read -r f0 d0 < <(bridge_frames)
  # Sample over the talking window: the joins take ~N×50 ms plus a 3 s settle.
  sleep $(( n / 20 + 6 ))
  t0=$(cpu_ticks "$bridge"); s0=$(date +%s.%N)
  m=$(murmur_cpu)
  sleep 20
  t1=$(cpu_ticks "$bridge"); s1=$(date +%s.%N)
  m2=$(murmur_cpu)
  wait "$load"
  read -r f1 d1 < <(bridge_frames)
  bridge_cpu=$(awk -v a="$t0" -v b="$t1" -v s="$s0" -v e="$s1" -v k="$ticks" 'BEGIN{printf "%.1f", 100*(b-a)/k/(e-s)}')
  rss=$(awk '/VmRSS/{print $2}' "/proc/$bridge/status")
  jq -c --arg m "$machine" --arg t "$stamp" --arg b "$bridge_cpu" --arg mm "$m" --arg mm2 "$m2" --arg rss "$rss" \
    --arg fw "$((f1 - f0))" --arg dr "$((d1 - d0))" \
    '. + {machine:$m, at:$t, murmur:"1.6.870", bridgeCpuPercent:($b|tonumber), bridgeRssKiB:($rss|tonumber), murmurCpuPercentSamples:[($mm|tonumber),($mm2|tonumber)], bridgeForwardedToBrowsers:($fw|tonumber), bridgeDropped:($dr|tonumber)}' \
    "$out/load-$n.json" | tee -a "$dest/results.jsonl"
  # Let every bridged user leave before the next step.
  sleep 3
done
