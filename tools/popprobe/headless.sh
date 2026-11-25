#!/usr/bin/env bash
# Scripted headless runs of the probe page (README.md "Headless"): starts
# popprobe, opens the page in a throwaway profile with ?autorun=, waits for
# the posted results and stops both. No WebDriver: the page posts its own
# result lines to popprobe.
#
#   headless.sh firefox|chrome <browser binary> loopback|public <relay> <certHash> [steps] [lan-http]
#
# loopback: the page is http://127.0.0.1:<port>, the most private address
#   space, so a LAN relay is a less-private target (no LNA gate expected).
# public:   the same page, marked public — Chrome by
#   --ip-address-space-overrides, Firefox by
#   network.lna.address_space.public.override — so the dial is
#   public → private, as from https://gawk.ioio.fi.
# steps:    comma list of dial, wrong, ping (default dial,wrong,ping).
# lan-http: popprobe's -lan-http address, the /ping control's target
#   (default <relay host>:8091).
#
# Environment: POPPROBE (a built popprobe; default `go run` here),
# POPPROBE_PORT (default 8090), POPPROBE_TIMEOUT (per step, s; default 15),
# POPPROBE_PREFS (extra Firefox user.js lines).
set -euo pipefail
browser=$1 bin=$2 mode=$3 relay=$4 hash=$5 steps=${6:-dial,wrong,ping}
host=${relay#https://}; host=${host%:*}; host=${host#[}; host=${host%]}
lan_http=${7:-$host:8091}
port=${POPPROBE_PORT:-8090}
per=${POPPROBE_TIMEOUT:-15}
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  sleep 1 # Chrome's helpers outlive the browser pid by a moment
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT

n=$(tr ',' '\n' <<<"$steps" | grep -c .)
if [ -n "${POPPROBE:-}" ]; then probe=("$POPPROBE"); else (cd "$here" && go build -o "$work/popprobe" .); probe=("$work/popprobe"); fi
"${probe[@]}" -addr "127.0.0.1:$port" -lan-http "$lan_http" -relay "$relay" -cert-hash "$hash" \
  -exit-after "$n" -out "$work/results.jsonl" >"$work/popprobe.log" 2>&1 &
probe_pid=$!; pids+=("$probe_pid")
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/config.json" >/dev/null && break; sleep 0.1; done

url="http://127.0.0.1:$port/?row=$browser-$mode-headless&autorun=$steps&timeout=$per&control=http://$lan_http/ping"
case $browser in
  firefox)
    mkdir -p "$work/profile"
    {
      echo 'user_pref("browser.shell.checkDefaultBrowser", false);'
      echo 'user_pref("datareporting.policy.dataSubmissionEnabled", false);'
      echo 'user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);'
      [ "$mode" = public ] && echo "user_pref(\"network.lna.address_space.public.override\", \"127.0.0.1:$port\");"
      printf '%s\n' "${POPPROBE_PREFS:-}"
    } >"$work/profile/user.js"
    "$bin" --headless --no-remote --profile "$work/profile" "$url" >"$work/browser.log" 2>&1 &
    ;;
  chrome)
    extra=()
    [ "$mode" = public ] && extra+=("--ip-address-space-overrides=127.0.0.1:$port=public")
    "$bin" --headless=new --no-first-run --no-default-browser-check --user-data-dir="$work/profile" \
      --enable-logging=stderr --v=0 ${extra[@]+"${extra[@]}"} "$url" >"$work/browser.log" 2>&1 &
    ;;
  *) echo "browser must be firefox or chrome" >&2; exit 2 ;;
esac
pids+=("$!")

echo "# $("$bin" --version 2>/dev/null | head -1), page $url"
deadline=$((SECONDS + n * (per + 10) + 20))
while kill -0 "$probe_pid" 2>/dev/null && [ $SECONDS -lt $deadline ]; do sleep 0.5; done
grep -E '^RESULT|ping ' "$work/popprobe.log" || true
# Chrome names the Local Network Access refusal only in its console.
grep -o 'net::ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS' "$work/browser.log" | sort | uniq -c | sed 's/^/# console: /' || true
kill -0 "$probe_pid" 2>/dev/null && { echo "# timed out waiting for $n result(s)" >&2; exit 1; }
exit 0
