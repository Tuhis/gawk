#!/usr/bin/env python3
"""Per-second sender counters from docs/88's lab runs (R84 BR2), for the
browser upload notice's table test (gawk-app/src/transport/
uplink-monitor.r84-fixture.ts). Reads each cell's send.csv from the inline
carrier page with RU9's retire rule (mode c151-inline-f12) and prints the
fixture.

    python3 tools/uplinklab/r84persec.py > gawk-app/src/transport/uplink-monitor.r84-fixture.ts

One row a second of the run: encoded frames (F lines), frames sent (F lines
whose frame was not shed), records shed (the chunk and parity records of each
shed frame, D lines), keyframe streams superseded (replaced, discarded or
aborted) and failed (aborted, failed or abandoned), by when their K line ended
them — the counts the browser's UplinkMonitor reads in Reliable mode (docs/88
D5). Sent is offered minus shed: the lab page logs no per-frame handover.
"""
import collections
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))
MODE = "c151-inline-f12"
CELLS = [
    "rtt10-clean", "rtt40-clean", "rtt100-clean", "rtt10-loss0.005", "rtt10-ge1pct",
    "rtt10-out80drop", "rtt40-out80drop", "rtt40-ge1pct", "rtt40-loss0.005", "rtt100-loss0.01",
]


def per_second(path):
    frames, shed, ends = {}, set(), []
    t0 = None
    with open(path) as f:
        for line in f:
            p = line.strip().split(",")
            if p[0] == "F":
                t = int(p[6])
                frames[int(p[1])] = (int(p[4]) + int(p[5]), t)
                t0 = t if t0 is None else t0
            elif p[0] == "D":
                shed.add(int(p[1]))
            elif p[0] == "K":
                ends.append((int(p[3]), p[4]))
    secs = collections.defaultdict(lambda: [0, 0, 0, 0, 0])
    for fid, (records, t) in frames.items():
        s = secs[(t - t0) // 1_000_000_000]
        s[0] += 1
        if fid in shed:
            s[2] += records
        else:
            s[1] += 1
    for t, outcome in ends:
        s = secs[(t - t0) // 1_000_000_000]
        if outcome in ("replaced", "discarded", "aborted"):
            s[3] += 1
        if outcome in ("aborted", "failed", "abandoned"):
            s[4] += 1
    return [secs[i] for i in range(max(secs) + 1)]


def main():
    print("// Per-second counters from docs/88's lab pass (2026-11-10, §4, table S2's")
    print("// `c151-inline-f12` runs: Chrome for Testing 151 sending through the inline")
    print("// carrier on RU9's retire rule), reduced from each run's send.csv by")
    print("// tools/uplinklab/r84persec.py. One row a second:")
    print("// [encoded frames, frames sent, records shed, keyframes superseded, keyframes failed].")
    print("// Generated; do not edit by hand.")
    print()
    print("export type LabSecond = readonly [number, number, number, number, number];")
    print()
    print("export const R84_LAB_SECONDS: Readonly<Record<string, readonly LabSecond[]>> = {")
    for cell in CELLS:
        rows = per_second(os.path.join(OUT, "results", cell, MODE, "send.csv"))
        print(f"  '{cell}': {json.dumps(rows, separators=(',', ':'))},")
    print("};")


if __name__ == "__main__":
    main()
