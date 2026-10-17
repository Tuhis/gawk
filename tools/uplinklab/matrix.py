#!/usr/bin/env python3
"""Runs uplink-lab scenarios (docs/73 §4): each scenario × delivery mode is
one real sender → impaired path → receiver run, then the transport analysis
and the viewer replay (live edge and adaptive). Results land in
$UPLINK_OUT/results/<scenario>/<mode>/; a cell with a summary.json is skipped,
so a set resumes where it stopped.

Usage: matrix.py core|cc|hi|jit|win|all [--jobs 3] [--seconds 100] [--only substr]
       matrix.py rescore [substr]   # replay every finished run again
"""
import concurrent.futures as cf
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))
APP = os.path.join(ROOT, "gawk-app")
REPLAY = "lab/uplink-replay.lab.test.ts"
TRACES = {"1080": f"{OUT}/traces/r1080p60.csv", "1440": f"{OUT}/traces/r1440p60.csv"}

MODES = {
    "dgram": ["--mode", "datagram"],
    "carrier": ["--mode", "carrier"],
    "dgram-bbr": ["--mode", "datagram", "--cc", "bbr"],
    "carrier-bbr": ["--mode", "carrier", "--cc", "bbr"],
    # ~100 ms of the default rung's peak: a queue becomes drops, not delay.
    "dgram-buf150k": ["--mode", "datagram", "--datagram-buffer", "153600"],
    "dgram-bbr-buf150k": ["--mode", "datagram", "--cc", "bbr", "--datagram-buffer", "153600"],
}


def scen(name, proxy, trace="1080", modes=("dgram", "carrier"), extra=(), recv=""):
    return dict(name=name, proxy=proxy, trace=trace, modes=modes, extra=list(extra), recv=recv)


def sets():
    s = {}
    core = []
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        core.append(scen(f"rtt{rtt}-clean", ["-rtt", r]))
        for loss in ("0.001", "0.005", "0.01", "0.02", "0.05"):
            core.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss]))
        # Bursty: ~1 % mean loss in bursts of ~4 packets.
        core.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"]))
        # AWDL-like: the radio away 20 or 80 ms of every second, both ways.
        core.append(scen(f"rtt{rtt}-out20drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "20ms"]))
        core.append(scen(f"rtt{rtt}-out80drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms"]))
        core.append(scen(f"rtt{rtt}-out80hold", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms",
                                                 "-outage-mode", "hold"]))
        # Episodic: 2 % loss for 10 s of every 40 s.
        core.append(scen(f"rtt{rtt}-episodic", ["-rtt", r, "-loss", "0.02", "-episode-period", "40s",
                                                "-episode-len", "10s"]))
    s["core"] = core
    # The congestion-controller and send-buffer variants, on the cells that
    # separate them.
    cc = []
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        variants = ("dgram-bbr", "carrier-bbr", "dgram-buf150k", "dgram-bbr-buf150k")
        cc.append(scen(f"rtt{rtt}-clean", ["-rtt", r], modes=variants))
        for loss in ("0.001", "0.005", "0.01", "0.02"):
            cc.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss], modes=variants))
        cc.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=variants))
        cc.append(scen(f"rtt{rtt}-out80drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms"],
                       modes=variants))
        cc.append(scen(f"rtt{rtt}-out20drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "20ms"],
                       modes=variants))
    s["cc"] = cc
    # The high rung (1440p60, 38.9 Mbps mean, 50 Mbps ceiling) and docs/57
    # D9's headroom table: a bottleneck at 300 or 80 Mbps behind the radio.
    hi = []
    both = ("dgram", "carrier", "dgram-bbr", "carrier-bbr")
    hi.append(scen("hi-rtt10-clean", ["-rtt", "10ms"], trace="1440", modes=both))
    hi.append(scen("hi-rtt10-loss0.001", ["-rtt", "10ms", "-loss", "0.001"], trace="1440", modes=both))
    hi.append(scen("hi-rtt10-ge1pct", ["-rtt", "10ms", "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], trace="1440",
                   modes=both))
    for rate in ("300e6", "80e6"):
        tag = rate.replace("e6", "M")
        hi.append(scen(f"hi-rtt10-rate{tag}", ["-rtt", "10ms", "-rate", rate, "-queue", "50ms"], trace="1440",
                       modes=both))
        hi.append(scen(f"hi-rtt10-rate{tag}-out80drop", ["-rtt", "10ms", "-rate", rate, "-queue", "50ms",
                                                         "-outage-period", "1070ms", "-outage-len", "80ms"],
                       trace="1440", modes=both))
    s["hi"] = hi
    # Robustness: the same comparisons on a path whose delay jitters (3 ms,
    # order kept), so neither controller is flattered by a constant RTT.
    jit = []
    four = ("dgram", "carrier", "dgram-bbr", "carrier-bbr")
    for rtt in (10, 40):
        r = f"{rtt}ms"
        base = ["-rtt", r, "-jitter", "3ms"]
        jit.append(scen(f"jit-rtt{rtt}-clean", base, modes=four))
        jit.append(scen(f"jit-rtt{rtt}-loss0.001", base + ["-loss", "0.001"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-loss0.005", base + ["-loss", "0.005"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-ge1pct", base + ["-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-out80drop", base + ["-outage-period", "1070ms", "-outage-len", "80ms"],
                        modes=four))
    s["jit"] = jit
    # D10: the relay's receive windows at the 50 Mbps ceiling (docs/57 D9's
    # rule), against quic-go's 512 KiB / 768 KiB defaults in "hi".
    win = [scen(f"win-hi-rtt10-{n}", p, trace="1440", modes=("carrier-bbr",),
                recv="-stream-window 2097152 -conn-window 3145728")
           for n, p in (("clean", ["-rtt", "10ms"]),
                        ("rate80M-out80drop", ["-rtt", "10ms", "-rate", "80e6", "-queue", "50ms",
                                               "-outage-period", "1070ms", "-outage-len", "80ms"]))]
    s["win"] = win
    s["all"] = core + cc + hi + jit + win
    return s


def replay(runs, skip_ms):
    """The viewer replay for each run dir, both presets, in one vitest
    process; then each run's summary.json is (re)written."""
    batch = f"{OUT}/replay-batch.{os.getpid()}.{id(runs)}.txt"
    with open(batch, "w") as b:
        for r in runs:
            for po in ("off", "adaptive"):
                b.write(f"{r}/vt.json {po} {r}/view-{po}.json\n")
    env = dict(os.environ, UPLINK_BATCH=batch, UPLINK_SKIP_MS=str(int(skip_ms)))
    subprocess.run(["npx", "vitest", "run", REPLAY, "--testTimeout", "3600000"], cwd=APP,
                   env=env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.remove(batch)
    for r in runs:
        view = {po: json.load(open(f"{r}/view-{po}.json")) for po in ("off", "adaptive")}
        t = json.load(open(f"{r}/transport.json"))
        json.dump(dict(transport=t, view=view, proxy=open(f"{r}/proxy.txt").read()),
                  open(f"{r}/summary.json", "w"), indent=1)


def run_one(sc, mode, seconds):
    out = f"{OUT}/results/{sc['name']}/{mode}"
    os.makedirs(out, exist_ok=True)
    if os.path.exists(f"{out}/summary.json"):
        return out
    args = [f"{HERE}/run.sh", out, "--trace", TRACES[sc["trace"]], "--seconds", str(seconds)]
    args += MODES[mode] + sc["extra"] + ["--"] + sc["proxy"] + ["-seed", "7"]
    env = dict(os.environ, RECV_ARGS=sc["recv"])
    subprocess.run(args, check=True, timeout=seconds + 120, env=env)
    subprocess.run(["python3", f"{HERE}/analyze.py", out, "--json", f"{out}/transport.json",
                    "--viewer-trace", f"{out}/vt.json"], check=True, stdout=subprocess.DEVNULL)
    # The second half of the run: the adaptive controller has settled.
    replay([out], seconds * 1000 / 2)
    return out


def rescore(flt):
    import glob
    runs = sorted(os.path.dirname(p) for p in glob.glob(f"{OUT}/results/*/*/transport.json"))
    runs = [r for r in runs if flt in r]
    replay(runs, 50000)
    print(len(runs), "rescored")


def main():
    which = sys.argv[1]
    if which == "rescore":
        rescore(sys.argv[2] if len(sys.argv) > 2 else "")
        return
    jobs, seconds, only = 3, 100, None
    a = sys.argv[2:]
    while a:
        if a[0] == "--jobs":
            jobs = int(a[1]); a = a[2:]
        elif a[0] == "--seconds":
            seconds = float(a[1]); a = a[2:]
        elif a[0] == "--only":
            only = a[1]; a = a[2:]
        else:
            raise SystemExit("bad arg")
    work = [(sc, m) for sc in sets()[which] for m in sc["modes"] if not only or only in sc["name"]]
    print(f"{len(work)} runs, {jobs} at a time, {seconds}s each", flush=True)
    with cf.ThreadPoolExecutor(jobs) as ex:
        futs = {ex.submit(run_one, sc, m, seconds): (sc["name"], m) for sc, m in work}
        for f in cf.as_completed(futs):
            n, m = futs[f]
            try:
                f.result()
                print("done", n, m, flush=True)
            except Exception as e:  # noqa: BLE001
                print("FAILED", n, m, e, flush=True)


if __name__ == "__main__":
    main()
