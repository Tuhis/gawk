#!/usr/bin/env python3
"""docs/76 §11.3 (R74 PP8): time to first frame from pop-ttff.sh's runs.

  pop-ttff.py <out-dir>...

Per run directory: the cold and warm joins' ttff_ms (nearest-rank p50 / p95,
and max), the viewer's own session_ms and the first delta after the
keyframe, and, from home's log, the leg's start after the viewer's
subscribe (want -> PoP -> /pop/publish/{id} handshake -> upgrade). Prints
markdown to stdout."""
import json
import math
import os
import re
import sys
from datetime import datetime


def rank(xs, p):
    xs = sorted(xs)
    return xs[max(0, math.ceil(p / 100 * len(xs)) - 1)] if xs else None


def stats(xs):
    if not xs:
        return "–"
    return f"{rank(xs, 50):.0f} / {rank(xs, 95):.0f} / {max(xs):.0f}"


def ts(line):
    m = re.match(r"time=(\S+)", line)
    return datetime.fromisoformat(m.group(1)).timestamp() * 1000 if m else None


def leg_starts(d):
    """ms from each cold viewer's subscribe to the leg's start at home."""
    out, sub = [], None
    for line in open(os.path.join(d, "home.log")):
        if 'msg="subscriber session started"' in line and sub is None:
            sub = ts(line)
        elif 'msg="pop uplink leg started"' in line and sub is not None:
            out.append(ts(line) - sub)
        elif 'reservation handed back' in line:
            sub = None
    return out


def host(d):
    def rd(n):
        p = os.path.join(d, n)
        return open(p).read().split()[0] if os.path.exists(p) else "–"
    return f"{rd('load.txt')} · {rd('steal.txt')} %"


print("| Run | Joins | Cold TTFF p50 / p95 / max ms | Cold: session up p50 | Cold: subscribe → leg up at home p50 / max ms "
      "| Cold: first delta after the keyframe p50 ms | Warm TTFF p50 / p95 / max ms | Load before · steal |")
print("|---|---|---|---|---|---|---|---|")
for d in sys.argv[1:]:
    rows = [json.loads(l) for l in open(os.path.join(d, "ttff.jsonl")) if l.strip()]
    cold = [r for r in rows if r["label"].startswith("cold")]
    warm = [r for r in rows if r["label"].startswith("warm")]
    ok = [r["ttff_ms"] for r in cold if "ttff_ms" in r]
    legs = leg_starts(d)
    gap = [r["first_delta_ms"] - r["ttff_ms"] for r in cold if "first_delta_ms" in r and "ttff_ms" in r]
    print(f"| {os.path.basename(d.rstrip('/'))} | {len(ok)}/{len(cold)} | {stats(ok)} | "
          f"{rank([r['session_ms'] for r in cold], 50):.0f} | "
          f"{(f'{rank(legs, 50):.0f} / {max(legs):.0f}') if legs else '–'} | "
          f"{(f'{rank(gap, 50):.0f}') if gap else '–'} | "
          f"{stats([r['ttff_ms'] for r in warm if 'ttff_ms' in r])} | {host(d)} |")
