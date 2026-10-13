#!/bin/bash
# Stops a booted CI Simulator's push daemon, apsd. On a runner it spins
# (there is no account and nothing to deliver), and the logs it writes keep
# the Simulator's diagnosticd and the host's busy as well: together they held
# 1-1.5 of the runner's three cores, idle or not (2026-10-13). The app uses
# no push: its Live Activity is local (pushType nil).
#
# Disabled first, so nothing that looks the service up starts it again, then
# booted out. One that is still running is a warning, not a failure: the
# tests are only slower for it.
#
#   gawk-ios/scripts/quiet-simulator.sh UDID
set -uo pipefail

udid="$1"
# The Simulator's own apsd, never the host's.
running='simruntime/.*ApplePushService\.framework/apsd'

labels="$(xcrun simctl spawn "$udid" launchctl list 2>/dev/null | awk '{print $3}' | grep -E '(^|\.)apsd$')"
echo "apsd's launchd labels: ${labels:-none listed}"
for label in ${labels:-com.apple.apsd}; do
  xcrun simctl spawn "$udid" launchctl disable "system/$label"
  xcrun simctl spawn "$udid" launchctl bootout "system/$label"
done
sleep 2
if pgrep -f "$running" > /dev/null; then
  echo "::warning::the Simulator's apsd is still running"
  pgrep -lf "$running"
else
  echo "the Simulator's apsd is stopped"
fi
