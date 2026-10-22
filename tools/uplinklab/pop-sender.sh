#!/usr/bin/env bash
# R74 PP8 (docs/76 item 18): the PoP's own sender for run.sh and matrix.py's
# pop-* modes: the hub's leg writer, driven by a synthetic publisher
# (gawk-uplinklab legsend). It takes the Rust sender's flags (--url --out
# --mode --trace --seconds --cc).
OUT=${UPLINK_OUT:-$(cd "$(dirname "$0")" && pwd)/out}
exec "$OUT/bin/gawk-uplinklab" legsend "$@"
