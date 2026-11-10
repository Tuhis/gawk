#!/usr/bin/env python3
"""The viewer replay at four presentation phases (docs/88 F1).

The replay (gawk-app/lab/uplink-replay.lab.test.ts) presents a 60 fps source
on an exact 60 Hz vsync, stepping its clock in 0.25 ms. Nothing drifts, so
the phase between frame targets and vsyncs is fixed for a whole run, and
when the targets sit within about a millisecond of the sink's half-vsync
due boundary, one vsync in three finds two frames due and shows only the
newer: 40 of 60 frames a second, without a stall, for as long as the
offset holds. Which runs land there is luck. This re-runs the replay with
the arrival trace shifted by a quarter vsync at a time (UPLINK_LEG_B_MS:
0 is the matrix's own replay; 4.17, 8.33 and 12.5 ms here), which moves the
phase and nothing else the viewer measures but a constant latency, and
writes view-<preset>-lb<shift>.json beside each run's view-<preset>.json.
r84tables.py reports the median of the four.

    python3 tools/uplinklab/phase.py [run-dir substring] [--jobs 3]
"""
import concurrent.futures as cf
import glob
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))
APP = os.path.join(ROOT, "gawk-app")
REPLAY = "lab/uplink-replay.lab.test.ts"
SHIFTS = ("4.17", "8.33", "12.5")
# The presets docs/88 reports (matrix.py PRESETS): trace, playout, delivery.
PRESETS = {
    "off": ("vt.json", "off", "live"),
    "adaptive": ("vt.json", "adaptive", "live"),
    "resilient": ("vt-noparity.json", "adaptive", "resilient"),
    "resilient-parity": ("vt.json", "adaptive", "resilient"),
    "deep-parity": ("vt.json", "adaptive", "deep"),
}


def replay(runs, shift, part):
    batch = f"{OUT}/phase-batch.{os.getpid()}.{shift}.{part}.txt"
    with open(batch, "w") as b:
        for r in runs:
            for name, (trace, po, delivery) in PRESETS.items():
                if not os.path.exists(f"{r}/view-{name}-lb{shift}.json"):
                    b.write(f"{r}/{trace} {po} {r}/view-{name}-lb{shift}.json {delivery}\n")
    if os.path.getsize(batch) > 0:
        env = dict(os.environ, UPLINK_BATCH=batch, UPLINK_SKIP_MS="50000", UPLINK_LEG_B_MS=shift)
        subprocess.run(["npx", "vitest", "run", REPLAY, "--testTimeout", "3600000"], cwd=APP, env=env,
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.remove(batch)


def main():
    a = sys.argv[1:]
    jobs = 3
    if "--jobs" in a:
        i = a.index("--jobs")
        jobs = int(a[i + 1])
        a = a[:i] + a[i + 2:]
    flt = a[0] if a else ""
    runs = sorted(os.path.dirname(p) for p in glob.glob(f"{OUT}/results/*/*/summary.json") if flt in p)
    work = [(s, i, runs[i::jobs]) for s in SHIFTS for i in range(jobs) if runs[i::jobs]]
    with cf.ThreadPoolExecutor(jobs) as ex:
        list(ex.map(lambda w: replay(w[2], w[0], w[1]), work))
    print(len(runs), "runs at", len(SHIFTS), "more phases")


if __name__ == "__main__":
    main()
