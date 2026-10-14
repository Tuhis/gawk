#!/bin/bash
# Waits for a CI runner to go quiet after a Simulator's first boot.
#
# `simctl bootstatus` answers at "booted", but the boot goes on for minutes
# after it: on the 3-core, 7 GB runner hundreds of the iOS 27 Simulator's
# daemons start, migrate and index, the 1-minute load average climbs to
# 400-500 and up to 230 processes are runnable at once, and it comes back
# under 30 about nine minutes after "booted" (two runs, 2026-10-13). A test,
# or a fixture's QUIC dial, started in that window can miss any deadline it
# has. This polls the load average every 10 s and returns once three
# readings in a row are under LIMIT; after MAX seconds it warns and returns
# anyway, since a busy runner makes a slower run, not a wrong one.
#
#   gawk-ios/scripts/settle.sh [LIMIT] [MAX]
set -euo pipefail

limit="${1:-40}"
max="${2:-900}"
quiet=0
waited=0
load=""
while [ "$waited" -lt "$max" ]; do
  load="$(sysctl -n vm.loadavg | awk '{print $2}')"
  echo "$(date -u +%H:%M:%S) load $load"
  if awk -v l="$load" -v m="$limit" 'BEGIN { exit !(l < m) }'; then
    quiet=$((quiet + 1))
  else
    quiet=0
  fi
  if [ "$quiet" -ge 3 ]; then
    echo "settled after $waited s"
    exit 0
  fi
  sleep 10
  waited=$((waited + 10))
done
echo "::warning::the runner was still busy after $max s (load $load); testing anyway"
