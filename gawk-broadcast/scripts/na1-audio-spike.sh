#!/usr/bin/env bash
#
# NA1 — R25 native-broadcaster audio spike (docs/28-native-broadcaster-audio.md).
#
# Answers, on one real Linux machine, the questions the R25 design deliberately
# left unverified. Every one of them changes what NA2/NA3/NA4 get written as, so
# guessing them is not an option:
#
#   Q1  Which audio source candidate actually resolves to the default sink's
#       monitor here — and does `pipewiresrc stream.capture.sink=true` forward
#       the property WirePlumber routes on? (Decision 2, risks 2 and 3)
#   Q2  Does `mpegtsmux` carry Opus in *this* pipeline, alongside H.264?
#       (Decision 4)
#   Q3  Does the muxed file round-trip through `tsdemux ! opusparse`?
#   Q4  What does the Opus PES payload's control header actually look like, byte
#       for byte? (risk 1 — the one thing NA4's demuxer is written against)
#   Q5  Does audio keep flowing while the video pad is nearly idle, or does the
#       muxer hold audio hostage to screen damage? (the GstAggregator risk)
#
# WHAT THIS DOES TO YOUR MACHINE: nothing that outlives it. It runs
# `gst-launch-1.0` pipelines, writes files into one output directory, and plays
# a quiet 440 Hz test tone through your default output for ~40 s (so there is
# something for the monitor capture to capture). No sudo, no packages
# installed, no settings changed, no network access, no screen capture — the
# share picker never appears, because the video side here is a test pattern,
# not your screen.
#
# Usage:
#   ./na1-audio-spike.sh                 # everything, ~3 minutes, unattended
#   ./na1-audio-spike.sh --follow-test   # + 25 s where you switch output device
#   ./na1-audio-spike.sh --no-tone       # you'll play your own audio instead
#   ./na1-audio-spike.sh --dry-run       # print every pipeline, run nothing
#   ./na1-audio-spike.sh --out DIR       # write results somewhere specific
#
# It ends by printing a summary and packing everything into a .tar.gz.

set -u

# ---------------------------------------------------------------------------
# Options and setup
# ---------------------------------------------------------------------------

TONE=1
DRY=0
FOLLOW=0
OUT=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--no-tone) TONE=0 ;;
	--follow-test) FOLLOW=1 ;;
	--dry-run)
		DRY=1
		TONE=0
		;;
	--out)
		OUT="${2:-}"
		shift
		;;
	-h | --help)
		sed -n '2,40p' "$0"
		exit 0
		;;
	*)
		echo "unknown option: $1 (try --help)" >&2
		exit 2
		;;
	esac
	shift
done

STAMP="$(date +%Y%m%d-%H%M%S)"
HOST="$(hostname 2>/dev/null || echo unknown)"
WORK="${OUT:-$PWD/na1-spike-$HOST-$STAMP}"
case "$WORK" in /*) ;; *) WORK="$PWD/$WORK" ;; esac
mkdir -p "$WORK/logs" "$WORK/media" || exit 1

# Every gst-launch pipeline below is one whitespace-separated string, so a
# `location=` carrying an absolute path dies the moment any directory in it has
# a space in it — gst-launch reads the second half as an element name and
# rejects the whole pipeline ("no element \"Desktop\"", run 1 2026-07-27, from
# ~/Downloads/Telegram Desktop/…). Working inside the output directory keeps
# every path in a pipeline a relative name this script chose itself.
cd "$WORK" || exit 1

CONSOLE="$WORK/console.log"
SUMMARY="$WORK/SUMMARY.txt"

say() { printf '%s\n' "$*" | tee -a "$CONSOLE"; }
hdr() {
	printf '\n=== %s ===\n' "$*" | tee -a "$CONSOLE"
}
have() { command -v "$1" >/dev/null 2>&1; }
have_el() {
	[[ $DRY == 1 ]] && return 0
	gst-inspect-1.0 "$1" >/dev/null 2>&1
}

# run <label> <timeout-secs> <command...>
#
# Everything a probe prints lands in its own log, prefixed by the exact command
# line, so a surprising result can be reproduced by hand without re-reading this
# script. SIGINT (not SIGTERM) on timeout: gst-launch -e turns SIGINT into EOS,
# which is what finalizes a file cleanly; -k force-kills a pipeline that ignores
# it.
run() {
	local label=$1 secs=$2
	shift 2
	local out="$WORK/logs/$label.log"
	{
		printf '$ %s\n\n' "$*"
	} >"$out"
	if [[ $DRY == 1 ]]; then
		printf '[%s]\n  %s\n\n' "$label" "$*" | tee -a "$CONSOLE"
		return 0
	fi
	timeout -s INT -k 3 "$secs" "$@" >>"$out" 2>&1
	local rc=$?
	printf '\n[exit %d]\n' "$rc" >>"$out"
	return $rc
}

# gst <label> <timeout-secs> <pipeline-string>
#
# The pipeline is a single whitespace-separated string. No token in any pipeline
# below contains a space, so plain word-splitting is exact — and it keeps the
# pipelines readable as the one thing that matters here.
gst() {
	local label=$1 secs=$2 pipeline=$3
	local -a argv
	read -ra argv <<<"$pipeline"
	run "$label" "$secs" gst-launch-1.0 "${argv[@]}"
}

if ! have gst-launch-1.0 && [[ $DRY == 0 ]]; then
	echo "gst-launch-1.0 not found. Install GStreamer first:" >&2
	echo "  Debian/Ubuntu: sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-base \\" >&2
	echo "                   gstreamer1.0-plugins-good gstreamer1.0-plugins-bad gstreamer1.0-pipewire" >&2
	echo "  Fedora:        sudo dnf install gstreamer1 gstreamer1-plugins-base gstreamer1-plugins-good \\" >&2
	echo "                   gstreamer1-plugins-bad-free gstreamer1-plugin-pipewire" >&2
	echo "  Arch:          sudo pacman -S gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad gst-plugin-pipewire" >&2
	exit 1
fi

PY=""
have python3 && PY=python3

say "NA1 audio spike — R25 (gawk-broadcast native audio)"
say "output: $WORK"
say ""

# ---------------------------------------------------------------------------
# The analyzer. Written out rather than shipped separately so this script is one
# file you can paste into a chat window; it also lands in the tarball, so every
# number below is traceable to the code that produced it.
# ---------------------------------------------------------------------------

cat >"$WORK/tsreport.py" <<'PYEOF'
#!/usr/bin/env python3
"""MPEG-TS / WAV analyzer for the NA1 spike.

Three modes:
  ts   <file>   structural report: PAT/PMT, PES streams, Opus control headers
  wav  <file>   level report: is there actually audio in this capture
  live          read a TS from stdin and report *arrival* cadence per stream

Only the standard library. Deliberately tolerant: this runs against output we
have never seen, so it prints what it finds rather than asserting a shape.
"""
import math
import os
import statistics
import sys
import time

PKT = 188

STREAM_TYPES = {
    0x02: "MPEG-2 video", 0x03: "MPEG-1 audio", 0x04: "MPEG-2 audio",
    0x06: "private data (PES)", 0x0F: "AAC ADTS", 0x11: "AAC LATM",
    0x1B: "H.264", 0x24: "HEVC", 0x81: "AC-3",
}


def iter_packets(buf):
    """Yield 188-byte TS packets, resyncing on the 0x47 sync byte."""
    i, n = 0, len(buf)
    while i + PKT <= n:
        if buf[i] != 0x47:
            i += 1
            continue
        yield buf[i:i + PKT]
        i += PKT


def split(pkt):
    """(pid, pusi, payload) for one packet; payload is b'' when absent."""
    pusi = bool(pkt[1] & 0x40)
    pid = ((pkt[1] & 0x1F) << 8) | pkt[2]
    afc = (pkt[3] >> 4) & 0x03
    off = 4
    if afc in (2, 3):
        off += 1 + pkt[4]
    if afc == 2 or off >= PKT:
        return pid, pusi, b""
    return pid, pusi, pkt[off:]


def descriptors(blob):
    """Parse a descriptor loop into (tag, bytes) pairs."""
    out, i = [], 0
    while i + 2 <= len(blob):
        tag, ln = blob[i], blob[i + 1]
        out.append((tag, blob[i + 2:i + 2 + ln]))
        i += 2 + ln
    return out


def describe_descriptor(tag, data):
    hexs = data.hex()
    if tag == 0x05:  # registration_descriptor
        return f"registration '{data.decode('ascii', 'replace')}' [{hexs}]"
    if tag == 0x7F:  # extension_descriptor
        ext = data[0] if data else -1
        if ext == 0x80:
            ch = data[1] if len(data) > 1 else -1
            return f"DVB extension 0x80 (Opus), channel_config={ch} [{hexs}]"
        return f"DVB extension 0x{ext:02x} [{hexs}]"
    return f"[{hexs}]"


def parse_psi(sections, pid, pusi, payload, out):
    """Accumulate and parse PAT/PMT sections (single- and multi-packet)."""
    if pusi:
        if not payload:
            return
        ptr = payload[0]
        sections[pid] = payload[1 + ptr:]
    elif pid in sections:
        sections[pid] += payload
    else:
        return
    sec = sections[pid]
    if len(sec) < 3:
        return
    length = ((sec[1] & 0x0F) << 8) | sec[2]
    if len(sec) < 3 + length:
        return  # wait for the rest
    table_id = sec[0]
    body = sec[3:3 + length - 4]  # minus CRC32
    if table_id == 0x00:  # PAT
        i = 5
        while i + 4 <= len(body):
            prog = (body[i] << 8) | body[i + 1]
            p = ((body[i + 2] & 0x1F) << 8) | body[i + 3]
            if prog:
                out["pmt_pids"][p] = prog
            i += 4
    elif table_id == 0x02:  # PMT
        pcr = ((body[5] & 0x1F) << 8) | body[6]
        pil = ((body[7] & 0x0F) << 8) | body[8]
        prog_desc = descriptors(body[9:9 + pil])
        i = 9 + pil
        streams = []
        while i + 5 <= len(body):
            st = body[i]
            epid = ((body[i + 1] & 0x1F) << 8) | body[i + 2]
            esl = ((body[i + 3] & 0x0F) << 8) | body[i + 4]
            streams.append((st, epid, descriptors(body[i + 5:i + 5 + esl])))
            i += 5 + esl
        out["pmt"] = {"pid": pid, "pcr": pcr, "prog_desc": prog_desc,
                      "streams": streams}


def pes_header(buf):
    """(stream_id, pts_or_None, payload_offset) for a PES packet start."""
    if len(buf) < 9 or buf[0:3] != b"\x00\x00\x01":
        return None, None, None
    sid = buf[3]
    # Only these stream_id values carry the optional PES header.
    if sid in (0xBC, 0xBE, 0xBF, 0xF0, 0xF1, 0xFF, 0xF2, 0xF8):
        return sid, None, 6
    flags = buf[7]
    hlen = buf[8]
    pts = None
    if flags & 0x80 and len(buf) >= 14:
        d = buf[9:14]
        pts = (((d[0] >> 1) & 0x07) << 30 | d[1] << 22 |
               (d[2] >> 1) << 15 | d[3] << 7 | d[4] >> 1)
    return sid, pts, 9 + hlen


TOC_MODES = []
for _c in range(32):
    if _c < 12:
        TOC_MODES.append(("SILK", ["NB", "MB", "WB"][_c // 4],
                          [10.0, 20.0, 40.0, 60.0][_c % 4]))
    elif _c < 16:
        TOC_MODES.append(("Hybrid", ["SWB", "FB"][(_c - 12) // 2],
                          [10.0, 20.0][(_c - 12) % 2]))
    else:
        TOC_MODES.append(("CELT", ["NB", "WB", "SWB", "FB"][(_c - 16) // 4],
                          [2.5, 5.0, 10.0, 20.0][(_c - 16) % 4]))


def decode_toc(b):
    mode, bw, ms = TOC_MODES[b >> 3]
    stereo = (b >> 2) & 1
    frames = {0: "1 frame", 1: "2 frames (equal)", 2: "2 frames (different)",
              3: "arbitrary (count in next byte)"}[b & 3]
    return (f"TOC 0x{b:02x}: config={b >> 3} {mode}/{bw} {ms} ms, "
            f"stereo={stereo}, {frames}")


def decode_opus_control(p):
    """Walk every Opus access unit in one PES payload.

    The control header is the load-bearing unknown NA4 is written against:

        prefix                 11 bits, value 0x3FF
        start_trim_flag         1 bit
        end_trim_flag           1 bit
        control_extension_flag  1 bit
        reserved                2 bits
        payload_size            8*N bits, 0xFF meaning "+255, continue"
        [start_trim 16 bits] [end_trim 16 bits] [control extension]

    0x3FF in an *11-bit* field is one zero bit followed by ten ones, so the
    header begins `7F Ex` — not `FF Ex`. (Verified against a real muxed file
    before this script was ever handed to anyone.)

    A PES may carry several access units back to back, so this returns one
    block of lines per unit rather than assuming one.
    """
    lines = []
    off, n = 0, 0
    while off + 2 <= len(p):
        if p[off] != 0x7F or (p[off + 1] & 0xE0) != 0xE0:
            lines.append(f"@{off}: control header sync MISSING (expected "
                         f"7F Ex, got {p[off]:02x} {p[off + 1]:02x}) — "
                         f"layout differs from the spec")
            break
        n += 1
        start_trim = (p[off + 1] >> 4) & 1
        end_trim = (p[off + 1] >> 3) & 1
        ctrl_ext = (p[off + 1] >> 2) & 1
        reserved = p[off + 1] & 0x03
        i, size = off + 2, 0
        while i < len(p):
            b = p[i]
            i += 1
            size += b
            if b != 0xFF:
                break
        extra = []
        if start_trim and i + 2 <= len(p):
            extra.append(f"start_trim={(p[i] << 8 | p[i + 1]) & 0x1FFF}")
            i += 2
        if end_trim and i + 2 <= len(p):
            extra.append(f"end_trim={(p[i] << 8 | p[i + 1]) & 0x1FFF}")
            i += 2
        if ctrl_ext and i < len(p):
            i += 1 + p[i]
        lines.append(f"AU {n} @{off}: header={i - off}B payload_size={size} "
                     f"flags(start_trim={start_trim} end_trim={end_trim} "
                     f"ctrl_ext={ctrl_ext} reserved={reserved}) "
                     + " ".join(extra))
        if i < len(p):
            lines.append("    " + decode_toc(p[i]))
        off = i + size
        if size == 0:
            break
    lines.append(f"=> {n} Opus access unit(s) in this PES "
                 f"({'ONE PER PES' if n == 1 else 'BATCHED — NA4 must split'})")
    return lines, n


def report_ts(path):
    buf = open(path, "rb").read()
    out = {"pmt_pids": {}, "pmt": None}
    sections = {}
    pes = {}   # pid -> dict
    order = []
    for pkt in iter_packets(buf):
        pid, pusi, payload = split(pkt)
        if pid == 0x0000 or pid in out["pmt_pids"]:
            parse_psi(sections, pid, pusi, payload, out)
            continue
        # 0x0000-0x001F are reserved for PSI (SDT, NIT, ...) and 0x1FFF is
        # stuffing: neither is an elementary stream, and counting them as one
        # would put phantom rows in the report.
        if not payload or pid <= 0x001F or pid == 0x1FFF:
            continue
        st = pes.setdefault(pid, {"count": 0, "sids": set(), "pts": [],
                                  "samples": [], "cur": None})
        if pusi:
            if st["cur"] is not None and len(st["samples"]) < 6:
                st["samples"].append(bytes(st["cur"]))
            st["count"] += 1
            sid, pts, off = pes_header(payload)
            if sid is not None:
                st["sids"].add(sid)
            if pts is not None:
                st["pts"].append(pts)
            st["cur"] = bytearray(payload[off:]) if off else None
            if pid not in order:
                order.append(pid)
        elif st["cur"] is not None and len(st["cur"]) < 4096:
            st["cur"] += payload
    for st in pes.values():
        if st["cur"] is not None and len(st["samples"]) < 6:
            st["samples"].append(bytes(st["cur"]))

    print(f"file: {path}  ({len(buf)} bytes, {len(buf) // PKT} TS packets)")
    print()
    print("--- PAT ---")
    for p, prog in out["pmt_pids"].items():
        print(f"  program {prog} -> PMT PID 0x{p:04x}")
    if not out["pmt_pids"]:
        print("  (none found)")
    print()
    audio_pids = []
    if out["pmt"]:
        m = out["pmt"]
        print(f"--- PMT (PID 0x{m['pid']:04x}) ---")
        print(f"  PCR PID 0x{m['pcr']:04x}")
        for tag, data in m["prog_desc"]:
            print(f"  program descriptor 0x{tag:02x}: "
                  f"{describe_descriptor(tag, data)}")
        for st, epid, descs in m["streams"]:
            name = STREAM_TYPES.get(st, "unknown")
            print(f"  stream_type 0x{st:02x} ({name})  PID 0x{epid:04x}")
            for tag, data in descs:
                print(f"      descriptor 0x{tag:02x}: "
                      f"{describe_descriptor(tag, data)}")
            is_opus = any(d == b"Opus" for t, d in descs if t == 0x05)
            if is_opus:
                audio_pids.append(epid)
                print("      ^ OPUS REGISTRATION FOUND")
    else:
        print("--- PMT --- (none found)")
    print()
    print("--- PES streams ---")
    for pid in order:
        st = pes[pid]
        sids = ", ".join(f"0x{s:02x}" for s in sorted(st["sids"]))
        line = (f"  PID 0x{pid:04x}  stream_id {sids}  "
                f"PES packets={st['count']}")
        if len(st["pts"]) > 2:
            d = [(b - a) / 90.0 for a, b in zip(st["pts"], st["pts"][1:])]
            d = [x for x in d if x > 0]
            if d:
                line += (f"  PTS delta ms: median={statistics.median(d):.1f} "
                         f"max={max(d):.1f}")
            span = (st["pts"][-1] - st["pts"][0]) / 90000.0
            line += f"  span={span:.2f}s"
        print(line)
    print()
    if audio_pids:
        for apid in audio_pids:
            st = pes.get(apid)
            if not st:
                continue
            print(f"--- Opus PES payloads (PID 0x{apid:04x}, first "
                  f"{len(st['samples'])}) ---")
            for n, s in enumerate(st["samples"], 1):
                print(f"  #{n} payload {len(s)} bytes: {s[:24].hex()}")
                for line in decode_opus_control(s)[0]:
                    print(f"      {line}")
            print()
    else:
        print("--- Opus PES payloads --- (no Opus stream identified)\n")
    print("VERDICT")
    print(f"  opus_in_pmt: {'yes' if audio_pids else 'no'}")
    print(f"  streams:     {len(order)}")


def report_wav(path):
    data = open(path, "rb").read()
    i = data.find(b"data")
    rate, ch, bits = 0, 0, 0
    f = data.find(b"fmt ")
    if f >= 0 and len(data) >= f + 24:
        ch = int.from_bytes(data[f + 10:f + 12], "little")
        rate = int.from_bytes(data[f + 12:f + 16], "little")
        bits = int.from_bytes(data[f + 22:f + 24], "little")
    if i < 0 or bits != 16:
        print(f"{os.path.basename(path)}: unreadable "
              f"(bytes={len(data)}, bits={bits})")
        return
    pcm = data[i + 8:]
    n = len(pcm) // 2
    if n == 0:
        print(f"{os.path.basename(path)}: NO SAMPLES")
        return
    peak, acc = 0, 0
    for k in range(0, n * 2, 2):
        v = int.from_bytes(pcm[k:k + 2], "little", signed=True)
        a = abs(v)
        if a > peak:
            peak = a
        acc += v * v
    rms = (acc / n) ** 0.5

    def dbfs(x):
        if x <= 0:
            return -99.0
        return round(20 * math.log10(x / 32768.0), 1)
    secs = n / max(rate * max(ch, 1), 1)
    print(f"{os.path.basename(path)}: {rate} Hz {ch}ch {secs:.2f}s  "
          f"peak={dbfs(peak)} dBFS  rms={dbfs(rms)} dBFS  "
          f"{'SILENT' if peak < 100 else 'AUDIO PRESENT'}")

    # A long capture is the default-sink-switch test: what matters is not the
    # overall level but whether the *end* still has audio in it. A source that
    # does not follow the default goes silent at the switch and averages out to
    # a perfectly respectable overall peak.
    if secs > 5:
        seg, parts = n // 4, []
        for k in range(4):
            p = 0
            for j in range(seg * k * 2, seg * (k + 1) * 2, 2):
                v = abs(int.from_bytes(pcm[j:j + 2], "little", signed=True))
                if v > p:
                    p = v
            parts.append(f"{dbfs(p)}")
        print(f"    quarters (peak dBFS, in time order): {' | '.join(parts)}"
              f"   <- all four loud = the source followed the switch")


def report_live():
    """Timestamp PES arrivals off stdin — the cadence the Go engine would see."""
    fd = sys.stdin.fileno()
    buf = b""
    aligned = False
    t0 = None
    arrivals = {}
    total = 0
    while True:
        try:
            # os.read, not BufferedReader.read: the latter would block until it
            # had a full request, which is precisely the timing we are measuring.
            chunk = os.read(fd, 8192)
        except OSError:
            break
        if not chunk:
            break
        now = time.monotonic()
        if t0 is None:
            t0 = now
        total += len(chunk)
        buf += chunk
        if not aligned:
            i = buf.find(b"\x47")
            if i < 0:
                buf = b""
                continue
            buf = buf[i:]
            aligned = True
        n = len(buf) // PKT * PKT
        for off in range(0, n, PKT):
            pkt = buf[off:off + PKT]
            if pkt[0] != 0x47:
                aligned = False   # resync on the next chunk
                n = off
                break
            pid, pusi, payload = split(pkt)
            if not pusi or not payload or pid == 0:
                continue
            if payload[0:3] == b"\x00\x00\x01":
                arrivals.setdefault(pid, []).append(now)
        buf = buf[n:]
    dur = (time.monotonic() - t0) if t0 else 0.0
    print(f"duration {dur:.1f}s  bytes {total}")
    for pid, ts in sorted(arrivals.items()):
        d = [(b - a) * 1000 for a, b in zip(ts, ts[1:])]
        if not d:
            print(f"  PID 0x{pid:04x}: {len(ts)} PES (too few for stats)")
            continue
        d.sort()
        p50 = d[len(d) // 2]
        p95 = d[min(len(d) - 1, int(len(d) * 0.95))]
        big = [x for x in d if x > 100]
        print(f"  PID 0x{pid:04x}: {len(ts)} PES  inter-arrival ms "
              f"p50={p50:.1f} p95={p95:.1f} max={d[-1]:.1f}  "
              f"gaps>100ms={len(big)}")


def report_rtp():
    """Arrival cadence of RFC 4571-framed RTP off stdin (the docs/28 fallback).

    Framing is `uint16 big-endian length` + RTP packet, which is what
    `rtpstreampay` writes. The RTP timestamp is the interesting column: at
    48 kHz with 20 ms frames it must step by exactly 960, and with
    `timestamp-offset=0` the first one tells us whether the payloader's clock
    base is the pipeline running time (shared with the muxer) or its own.
    """
    fd = sys.stdin.fileno()
    buf = b""
    t0 = None
    arrivals, stamps, seqs = [], [], []
    total = 0
    while True:
        try:
            chunk = os.read(fd, 8192)
        except OSError:
            break
        if not chunk:
            break
        now = time.monotonic()
        if t0 is None:
            t0 = now
        total += len(chunk)
        buf += chunk
        while len(buf) >= 2:
            ln = int.from_bytes(buf[:2], "big")
            if ln == 0 or ln > 4096:
                buf = buf[1:]      # not a plausible frame; resync
                continue
            if len(buf) < 2 + ln:
                break
            pkt, buf = buf[2:2 + ln], buf[2 + ln:]
            if len(pkt) >= 12 and (pkt[0] >> 6) == 2:
                arrivals.append(now)
                seqs.append(int.from_bytes(pkt[2:4], "big"))
                stamps.append(int.from_bytes(pkt[4:8], "big"))
    dur = (time.monotonic() - t0) if t0 else 0.0
    print(f"duration {dur:.1f}s  bytes {total}  RTP packets {len(arrivals)}")
    if len(arrivals) < 2:
        print("  (too few packets for stats)")
        return
    d = sorted((b - a) * 1000 for a, b in zip(arrivals, arrivals[1:]))
    p50 = d[len(d) // 2]
    p95 = d[min(len(d) - 1, int(len(d) * 0.95))]
    print(f"  inter-arrival ms p50={p50:.1f} p95={p95:.1f} max={d[-1]:.1f}  "
          f"gaps>100ms={len([x for x in d if x > 100])}")
    ts_d = [b - a for a, b in zip(stamps, stamps[1:])]
    lost = sum(1 for a, b in zip(seqs, seqs[1:]) if (b - a) & 0xFFFF != 1)
    print(f"  RTP timestamp step: median={statistics.median(ts_d)} "
          f"(960 == 20 ms @ 48 kHz)   first timestamp={stamps[0]}")
    print(f"  sequence discontinuities: {lost}")


if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    if mode == "ts":
        report_ts(sys.argv[2])
    elif mode == "wav":
        report_wav(sys.argv[2])
    elif mode == "live":
        report_live()
    elif mode == "rtp":
        report_rtp()
    else:
        print(__doc__)
        sys.exit(2)
PYEOF

# ---------------------------------------------------------------------------
# Step 0 — environment
# ---------------------------------------------------------------------------

hdr "Step 0: environment"

{
	echo "date:      $(date -Is)"
	echo "host:      $HOST"
	echo "kernel:    $(uname -srmo)"
	echo "distro:    $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
	echo "session:   ${XDG_SESSION_TYPE:-?} / ${XDG_CURRENT_DESKTOP:-?}"
	echo "gstreamer: $(gst-launch-1.0 --version | head -1)"
	echo "pipewire:  $(pipewire --version 2>&1 | head -1)"
	echo "wireplumb: $(wireplumber --version 2>&1 | head -1)"
	echo
	echo "-- elements --"
	for el in pipewiresrc pulsesrc alsasrc audiotestsrc audioconvert audioresample \
		opusenc opusparse mpegtsmux tsdemux h264parse videotestsrc \
		vulkanh264enc nvh264enc vah264enc x264enc openh264enc identity fakesink \
		tee rtpopuspay rtpstreampay; do
		if have_el "$el"; then echo "  yes  $el"; else echo "  NO   $el"; fi
	done
	echo
	echo "-- pactl --"
	pactl info 2>&1 | sed 's/^/  /'
	echo "  default sink: $(pactl get-default-sink 2>&1)"
	echo "  -- sources --"
	pactl list short sources 2>&1 | sed 's/^/  /'
	echo "  -- sinks --"
	pactl list short sinks 2>&1 | sed 's/^/  /'
} >"$WORK/logs/00-env.log" 2>&1

sed -n '1,9p' "$WORK/logs/00-env.log" | tee -a "$CONSOLE"

DEFAULT_SINK="$(pactl get-default-sink 2>/dev/null || true)"
[[ $DRY == 1 && -z "$DEFAULT_SINK" ]] && DEFAULT_SINK="alsa_output.example"
say "  default sink: ${DEFAULT_SINK:-<pactl unavailable>}"

# Property surfaces we are about to depend on. If `stream-properties` is absent
# from this pipewiresrc, candidate 1 is not merely failing — it cannot exist.
have_el pipewiresrc && gst-inspect-1.0 pipewiresrc >"$WORK/logs/01-inspect-pipewiresrc.log" 2>&1
have_el opusenc && gst-inspect-1.0 opusenc >"$WORK/logs/01-inspect-opusenc.log" 2>&1
have_el mpegtsmux && gst-inspect-1.0 mpegtsmux >"$WORK/logs/01-inspect-mpegtsmux.log" 2>&1

PW_HAS_STREAMPROPS=no
grep -q "stream-properties" "$WORK/logs/01-inspect-pipewiresrc.log" 2>/dev/null && PW_HAS_STREAMPROPS=yes
MUX_TAKES_OPUS=no
grep -q "audio/x-opus" "$WORK/logs/01-inspect-mpegtsmux.log" 2>/dev/null && MUX_TAKES_OPUS=yes
say "  pipewiresrc has stream-properties: $PW_HAS_STREAMPROPS"
say "  mpegtsmux sink caps mention audio/x-opus: $MUX_TAKES_OPUS"

# ---------------------------------------------------------------------------
# Step 1 — audio source candidates (Q1)
# ---------------------------------------------------------------------------

hdr "Step 1: audio source candidates"

TONE_PID=""
TONE_OK=skipped
cleanup() {
	[[ -n "$TONE_PID" ]] && kill "$TONE_PID" 2>/dev/null
	return 0
}
trap cleanup EXIT

if [[ $TONE == 1 ]]; then
	# Something has to be coming out of the speakers, or every candidate
	# "succeeds" while capturing digital silence — which looks exactly like a
	# candidate that resolved to the wrong node.
	gst-launch-1.0 -q audiotestsrc is-live=true wave=sine freq=440 volume=0.12 \
		! audioconvert ! autoaudiosink >"$WORK/logs/tone.log" 2>&1 &
	TONE_PID=$!
	sleep 1.5
	if kill -0 "$TONE_PID" 2>/dev/null; then TONE_OK=yes; else TONE_OK=no; fi
	say "  test tone playing (440 Hz, quiet): $TONE_OK"
	[[ $TONE_OK == no ]] && say "  !! tone failed to start — see logs/tone.log; play music manually"
else
	say "  --no-tone: play some audio yourself now, and keep it playing"
	sleep 3
fi

AUDIO_CAPS="audio/x-raw,rate=48000,channels=2"
WAV_CAPS="audio/x-raw,rate=48000,channels=2,format=S16LE"

# Candidate list. Name | source element tokens | note.
# A1/A2 differ only in how the boolean is spelled into the PipeWire props —
# risk 2 is about whether WirePlumber sees the property at all, and a
# GstStructure that deserializes it as a gboolean rather than a string is the
# likeliest way for it to silently not arrive.
declare -a CAND_NAME CAND_SRC CAND_NOTE
add_cand() {
	CAND_NAME+=("$1")
	CAND_SRC+=("$2")
	CAND_NOTE+=("$3")
}

add_cand A1 "pipewiresrc stream-properties=props,stream.capture.sink=true ! audio/x-raw" \
	"docs/28 candidate 1 (bool), caps-pinned to audio"
add_cand A2 "pipewiresrc stream-properties=props,stream.capture.sink=(string)true ! audio/x-raw" \
	"docs/28 candidate 1, property spelled as a string"
add_cand A3 "pipewiresrc stream-properties=props,stream.capture.sink=true" \
	"docs/28 candidate 1, no caps filter (does it negotiate audio on its own?)"
add_cand B "pulsesrc device=@DEFAULT_MONITOR@" \
	"docs/28 candidate 2 (pipewire-pulse compatibility)"
if [[ -n "$DEFAULT_SINK" ]]; then
	add_cand C "pulsesrc device=$DEFAULT_SINK.monitor" \
		"docs/28 candidate 3 (explicit device)"
	add_cand D "pipewiresrc target-object=$DEFAULT_SINK stream-properties=props,stream.capture.sink=true ! audio/x-raw" \
		"pipewiresrc pinned to the current default sink (does not follow a switch)"
fi

WINNER=""
WINNER_NOTE=""
: >"$WORK/logs/10-candidates.txt"
for i in "${!CAND_NAME[@]}"; do
	name="${CAND_NAME[$i]}"
	src="${CAND_SRC[$i]}"
	wav_rel="media/cand-$name.wav"
	wav="$WORK/$wav_rel"

	# (a) capture real PCM, so "it ran" and "it captured the speakers" are two
	#     different findings rather than one assumption.
	gst "11-cand-$name-wav" 5 "-q -e $src ! audioconvert ! audioresample ! $WAV_CAPS ! wavenc ! filesink location=$wav_rel"
	rc_wav=$?

	# (b) the exact trial shape docs/28 NA2 proposes, with `identity eos-after`
	#     rather than the doc's `fakesink num-buffers` (fakesink has no such
	#     property — GstBaseSrc does). Both spellings are tried, so the doc gets
	#     corrected from evidence rather than from memory.
	gst "12-cand-$name-trial" 8 "-q $src ! audioconvert ! audioresample ! $AUDIO_CAPS ! opusenc ! identity eos-after=25 ! fakesink"
	rc_trial=$?

	level="(not analysed)"
	if [[ -n "$PY" && -s "$wav" ]]; then
		level="$($PY "$WORK/tsreport.py" wav "$wav" 2>&1)"
	fi
	verdict="FAIL"
	if [[ $DRY == 1 ]] || { [[ -s "$wav" ]] && [[ $rc_trial == 0 ]]; }; then verdict="OK"; fi

	{
		echo "[$name] ${CAND_NOTE[$i]}"
		echo "     source:   $src"
		echo "     wav rc=$rc_wav trial rc=$rc_trial -> $verdict"
		echo "     level:    $level"
		echo "     stderr:   $(grep -m2 -iE 'error|warning|not-negotiated|no property|not found' "$WORK/logs/11-cand-$name-wav.log" | tr '\n' ' ' | cut -c1-200)"
		echo
	} >>"$WORK/logs/10-candidates.txt"

	say "  $name  $verdict  ${level#*: }"
	if [[ -z "$WINNER" && $verdict == OK ]] && ! echo "$level" | grep -q SILENT; then
		WINNER="$src"
		WINNER_NOTE="$name — ${CAND_NOTE[$i]}"
	fi
done

# The doc's literal trial spelling, once, so the correction is on the record.
if [[ -n "$WINNER" ]]; then
	gst "13-trial-doc-spelling" 8 "-q $WINNER ! audioconvert ! audioresample ! $AUDIO_CAPS ! opusenc ! fakesink num-buffers=25"
	DOC_TRIAL_RC=$?
else
	DOC_TRIAL_RC="skipped"
fi

if [[ -z "$WINNER" ]]; then
	say "  !! no candidate produced audible audio — the mux steps will use audiotestsrc"
	WINNER="audiotestsrc is-live=true wave=sine freq=440"
	WINNER_NOTE="NONE — fell back to audiotestsrc, so Q2–Q5 stay answerable"
fi
say "  winner: $WINNER_NOTE"

# ---------------------------------------------------------------------------
# Step 2 — pick an H.264 encoder for the mux tests
# ---------------------------------------------------------------------------

hdr "Step 2: H.264 encoder for the mux test"

# Production order first (docs/19 Cascade), then software — the mux question is
# indifferent to which encoder feeds it, but a hardware one keeps the test the
# same shape as the real pipeline.
video_branch() { # <encoder> <framerate-fraction, e.g. 30/1 or 1/5> [element to insert after the caps]
	local enc=$1 rate=$2 insert=${3:-}
	local caps="video/x-raw,width=1280,height=720,framerate=${rate}"
	local gpu="width=1280,height=720,framerate=${rate}"
	[[ -n "$insert" ]] && caps="$caps ! $insert"
	case "$enc" in
	vulkanh264enc) echo "$caps ! vulkanupload ! vulkancolorconvert ! video/x-raw(memory:VulkanImage),$gpu ! vulkanh264enc rate-control=cbr bitrate=8000" ;;
	nvh264enc) echo "$caps ! cudaupload ! cudaconvertscale ! video/x-raw(memory:CUDAMemory),$gpu ! nvh264enc rc-mode=vbr zerolatency=true bitrate=6000 max-bitrate=8000 bframes=0" ;;
	vah264enc) echo "$caps ! vapostproc ! video/x-raw(memory:VAMemory),$gpu ! vah264enc rate-control=vbr bitrate=8000 target-percentage=75 b-frames=0" ;;
	x264enc) echo "$caps ! videoconvert ! x264enc tune=zerolatency bitrate=8000 bframes=0" ;;
	openh264enc) echo "$caps ! videoconvert ! openh264enc bitrate=8000000" ;;
	esac
}

ENC=""
for cand in vulkanh264enc nvh264enc vah264enc x264enc openh264enc; do
	have_el "$cand" || continue
	gst "20-enctrial-$cand" 12 "-q videotestsrc num-buffers=20 ! $(video_branch "$cand" 30/1) ! h264parse ! fakesink"
	if [[ $? == 0 ]]; then
		ENC="$cand"
		break
	fi
done
if [[ -z "$ENC" ]]; then
	say "  !! no H.264 encoder encoded a test pattern — Q2/Q4 will be audio-only"
else
	say "  encoder: $ENC"
fi

OPUS_ENC="opusenc bitrate=128000 frame-size=20 dtx=false inband-fec=false audio-type=restricted-lowdelay"

# ---------------------------------------------------------------------------
# Step 3 — mux Opus + H.264 into MPEG-TS (Q2), round-trip it (Q3), read the
#          control header (Q4)
# ---------------------------------------------------------------------------

hdr "Step 3: mux Opus into MPEG-TS"

TS_REL="media/both.ts"
TS="$WORK/$TS_REL"
if [[ -n "$ENC" ]]; then
	MUX="-q -e mpegtsmux name=mux ! filesink location=$TS_REL videotestsrc is-live=true pattern=smpte ! $(video_branch "$ENC" 30/1) ! h264parse config-interval=-1 ! mux. $WINNER ! queue ! audioconvert ! audioresample ! $AUDIO_CAPS ! $OPUS_ENC ! queue ! mux."
else
	MUX="-q -e mpegtsmux name=mux ! filesink location=$TS_REL $WINNER ! queue ! audioconvert ! audioresample ! $AUDIO_CAPS ! $OPUS_ENC ! queue ! mux."
fi
gst "30-mux" 12 "$MUX"
MUX_RC=$?
TS_BYTES=$(stat -c %s "$TS" 2>/dev/null || echo 0)
say "  muxed: $TS_BYTES bytes (exit $MUX_RC)"

# Did the encoder properties we intend to ship even parse? A rejected property
# is a startup failure in production, and it reads identically to a broken
# source unless it is isolated here.
gst "31-opusenc-props" 8 "-q audiotestsrc num-buffers=60 ! audioconvert ! audioresample ! $AUDIO_CAPS ! $OPUS_ENC ! fakesink"
OPUS_PROPS_RC=$?
say "  opusenc property set accepted: $([[ $OPUS_PROPS_RC == 0 ]] && echo yes || echo NO)"

ROUNDTRIP="no"
if [[ $DRY == 1 || $TS_BYTES -gt 0 ]]; then
	gst "32-roundtrip" 15 "-v filesrc location=$TS_REL ! tsdemux name=d d. ! queue ! opusparse ! fakesink d. ! queue ! h264parse ! fakesink"
	if grep -q "audio/x-opus" "$WORK/logs/32-roundtrip.log" 2>/dev/null; then ROUNDTRIP="yes"; fi
	say "  tsdemux ! opusparse round-trip: $ROUNDTRIP"
	if have gst-discoverer-1.0; then
		run "33-discoverer" 20 gst-discoverer-1.0 "$TS"
	fi
	if [[ -n "$PY" ]]; then
		$PY "$WORK/tsreport.py" ts "$TS" >"$WORK/logs/34-ts-report.txt" 2>&1
		grep -E "OPUS REGISTRATION|stream_type|control header|TOC 0x" "$WORK/logs/34-ts-report.txt" |
			head -12 | sed 's/^/  /' | tee -a "$CONSOLE"
	fi
fi

# ---------------------------------------------------------------------------
# Step 4 — arrival cadence (Q5): does audio flow while video is nearly idle?
# ---------------------------------------------------------------------------

hdr "Step 4: arrival cadence"

# The measurement that matters is *arrival at our end of the pipe*, which is
# exactly what the Go engine sees — not PTS in a file, which would look perfect
# even if the muxer emitted a whole second of audio in one burst.
cadence() { # <label> <seconds> <pipeline> [analyzer-mode, default live]
	local label=$1 secs=$2 pipeline=$3 mode=${4:-live}
	local -a argv
	read -ra argv <<<"$pipeline"
	printf '$ gst-launch-1.0 %s\n\n' "$pipeline" >"$WORK/logs/$label.txt"
	if [[ $DRY == 1 ]]; then
		printf '[%s]\n  gst-launch-1.0 %s\n\n' "$label" "$pipeline" | tee -a "$CONSOLE"
		return
	fi
	if [[ -z "$PY" ]]; then
		echo "(python3 missing — cadence not measured)" >>"$WORK/logs/$label.txt"
		return
	fi
	timeout -s INT -k 3 "$secs" gst-launch-1.0 "${argv[@]}" 2>"$WORK/logs/$label.stderr" |
		$PY "$WORK/tsreport.py" "$mode" >>"$WORK/logs/$label.txt" 2>&1
	tail -n +3 "$WORK/logs/$label.txt" | sed 's/^/  /' | tee -a "$CONSOLE"
}

AUDIO_BRANCH="$WINNER ! queue ! audioconvert ! audioresample ! $AUDIO_CAPS ! $OPUS_ENC ! queue ! mux."

if [[ -n "$ENC" ]]; then
	say "  (a) video at 30 fps — the control"
	cadence "40-cadence-30fps" 12 \
		"-q mpegtsmux name=mux ! fdsink fd=1 videotestsrc is-live=true pattern=smpte ! $(video_branch "$ENC" 30/1) ! h264parse config-interval=-1 ! mux. $AUDIO_BRANCH"

	# One video frame every 5 s stands in for a static screen. It is a proxy,
	# not the real thing — portal capture emits *nothing* while nothing moves —
	# but it is the same question asked of the aggregator: can audio leave the
	# muxer without a video buffer arriving to push it out?
	#
	# Run 1 (2026-07-27) answered: no. Audio came out in ~5 s bursts aligned to
	# the video buffers, max gap 4999 ms, where the same audio branch with *no*
	# video pad ran at a clean 21 ms.
	say "  (b) video at 1 frame / 5 s — the static-screen proxy"
	cadence "41-cadence-static" 22 \
		"-q mpegtsmux name=mux ! fdsink fd=1 videotestsrc is-live=true pattern=smpte ! $(video_branch "$ENC" 1/5) ! h264parse config-interval=-1 ! mux. $AUDIO_BRANCH"

	# The same starvation, reached a different way — and this is the one that
	# decides whether run 1's result indicts the muxer or the proxy. Declaring
	# framerate=1/5 makes each video buffer 5 s long and gives the encoder a
	# 5-second frame interval, so "the muxer waits for the video pad" and "the
	# whole live pipeline's latency is one video frame = 5 s" produce identical
	# bursts. Here the caps stay 30 fps — buffer durations 33 ms, encoder
	# latency one 30 fps frame — and the frames are simply *dropped* before the
	# converter, which is what damage-driven capture looks like. If audio still
	# bursts, the muxer gates audio on video arrival and Decision 4's shared-mux
	# design is unsound. If audio flows, run 1's burst was an artifact of the
	# proxy and Decision 4 survives.
	say "  (b2) video at 30 fps with 97% of frames dropped — the same starvation, short buffers"
	cadence "43-cadence-sparse" 22 \
		"-q mpegtsmux name=mux ! fdsink fd=1 videotestsrc is-live=true pattern=smpte ! $(video_branch "$ENC" 30/1 "identity drop-probability=0.97") ! h264parse config-interval=-1 ! mux. $AUDIO_BRANCH"
fi

say "  (c) audio only — the muxer's own floor"
cadence "42-cadence-audioonly" 10 \
	"-q mpegtsmux name=mux ! fdsink fd=1 $AUDIO_BRANCH"

# docs/28 Decision 4's pre-registered fallback, measured rather than assumed:
# audio leaves the same child on its own pipe as RFC 4571-framed RTP while the
# TS mux keeps the video. Run it under the *starved* video branch, because the
# only question worth asking is whether this path keeps flowing where the muxed
# one stalls.
#
# It also tests something the doc got wrong on its face: the fallback is
# described as costing "two timebases instead of one", but a second *pipe* out
# of the same GStreamer pipeline is not a second *clock* — both branches are
# stamped from one pipeline running time. Whether that survives into the RTP
# timestamps is exactly what the rtp analyzer prints.
if [[ -n "$ENC" ]] && have_el rtpopuspay && have_el rtpstreampay; then
	say "  (d) fallback shape: Opus on its own pipe (RTP/RFC 4571) with video starved"
	cadence "44-fallback-rtp" 22 \
		"-q mpegtsmux name=mux ! fakesink videotestsrc is-live=true pattern=smpte ! $(video_branch "$ENC" 30/1 "identity drop-probability=0.97") ! h264parse config-interval=-1 ! mux. $WINNER ! queue ! audioconvert ! audioresample ! $AUDIO_CAPS ! $OPUS_ENC ! tee name=t t. ! queue ! mux. t. ! queue ! rtpopuspay timestamp-offset=0 ! rtpstreampay ! fdsink fd=1" \
		rtp
fi

# ---------------------------------------------------------------------------
# Step 5 — optional: does the winning source follow a default-sink switch?
# ---------------------------------------------------------------------------

FOLLOW_RESULT="not run (--follow-test)"
if [[ $FOLLOW == 1 && $DRY == 0 ]]; then
	hdr "Step 5: default output switch (needs you at the keyboard)"
	fwav="$WORK/media/follow.wav"
	fwav_rel="media/follow.wav"
	say "  Recording 24 s from the winning source, starting NOW."
	say ""
	say "  >>> After about 8 seconds, switch your default output device. <<<"
	say "      (Sound settings: speakers <-> headphones <-> HDMI. Or just"
	say "       plug/unplug headphones.) Keep the tone playing throughout."
	say ""
	gst "50-follow" 28 "-q -e $WINNER ! audioconvert ! audioresample ! $WAV_CAPS ! wavenc ! filesink location=$fwav_rel"
	if [[ -n "$PY" && -s "$fwav" ]]; then
		FOLLOW_RESULT="$($PY "$WORK/tsreport.py" wav "$fwav" 2>&1)"
	else
		FOLLOW_RESULT="capture failed (see logs/50-follow.log)"
	fi
	printf '%s\n' "$FOLLOW_RESULT" | sed 's/^/  /' | tee -a "$CONSOLE"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

opus_in_pmt=unknown
ctrl_hdr="(not decoded)"
if [[ -f "$WORK/logs/34-ts-report.txt" ]]; then
	grep -q "opus_in_pmt: yes" "$WORK/logs/34-ts-report.txt" && opus_in_pmt=yes
	grep -q "opus_in_pmt: no" "$WORK/logs/34-ts-report.txt" && opus_in_pmt=no
	ctrl_hdr="$(grep -m1 "control header" "$WORK/logs/34-ts-report.txt" | sed 's/^ *//')"
fi

{
	echo "NA1 spike summary — R25 native broadcaster audio"
	echo "host $HOST   $(date -Is)"
	echo "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")  |  ${XDG_SESSION_TYPE:-?}/${XDG_CURRENT_DESKTOP:-?}"
	echo "$(gst-launch-1.0 --version | head -1)  |  pipewire $(pipewire --version 2>&1 | head -1 | awk '{print $NF}')"
	echo
	echo "Q1  audio source"
	echo "    winner:        ${WINNER_NOTE}"
	echo "    test tone:     $TONE_OK"
	# A candidate that opens and records digital silence is indistinguishable
	# from one that resolved to the wrong node — unless we know something was
	# playing. Saying so is the difference between a result and a non-result.
	if [[ $TONE_OK != yes ]] && ! grep -q "AUDIO PRESENT" "$WORK/logs/10-candidates.txt" 2>/dev/null; then
		echo "    !! NOTHING AUDIBLE WAS CAPTURED and the test tone did not run."
		echo "       Q1 is INCONCLUSIVE: every candidate may simply have had"
		echo "       silence to record. Re-run without --no-tone, or with music"
		echo "       playing, before believing any of the levels below."
	fi
	echo "    stream-properties on pipewiresrc: $PW_HAS_STREAMPROPS"
	sed 's/^/    /' "$WORK/logs/10-candidates.txt"
	echo "Q2  mpegtsmux carries Opus"
	echo "    sink caps mention audio/x-opus: $MUX_TAKES_OPUS"
	echo "    encoder used for video:         ${ENC:-none}"
	echo "    muxed file:                     $TS_BYTES bytes"
	echo "    Opus in PMT:                    $opus_in_pmt"
	echo "    opusenc property set accepted:  $([[ ${OPUS_PROPS_RC:-1} == 0 ]] && echo yes || echo NO)"
	echo "    doc's 'fakesink num-buffers=25' trial spelling: exit ${DOC_TRIAL_RC}"
	echo
	echo "Q3  round-trip through tsdemux ! opusparse: $ROUNDTRIP"
	echo
	echo "Q4  Opus PES control header"
	echo "    $ctrl_hdr"
	if [[ -f "$WORK/logs/34-ts-report.txt" ]]; then
		grep -A3 "^  #1 payload" "$WORK/logs/34-ts-report.txt" | sed 's/^/    /'
	fi
	echo
	echo "Q5  arrival cadence (does audio survive an idle video pad?)"
	echo "    Read it as: the lower-numbered PID is video, the busier one audio."
	echo "    40 (30 fps) and 42 (no video) are the controls — audio must be ~20 ms"
	echo "    there. 41 and 43 starve the video pad two different ways; 44 is the"
	echo "    docs/28 fallback measured under the same starvation."
	for f in 40-cadence-30fps 41-cadence-static 43-cadence-sparse 42-cadence-audioonly 44-fallback-rtp; do
		[[ -f "$WORK/logs/$f.txt" ]] || continue
		echo "    -- $f"
		tail -n +3 "$WORK/logs/$f.txt" | sed 's/^/      /'
	done
	echo
	echo "Q6  default output switch (docs/28 goal criterion 2, --follow-test)"
	printf '%s\n' "$FOLLOW_RESULT" | sed 's/^/    /'
	echo
	echo "Full logs and media are in the tarball beside this file."
} >"$SUMMARY"

hdr "Summary"
cat "$SUMMARY" | tee -a "$CONSOLE" >/dev/null
sed -n '1,12p' "$SUMMARY"

TARBALL="$WORK.tar.gz"
tar czf "$TARBALL" -C "$(dirname "$WORK")" "$(basename "$WORK")" 2>/dev/null

say ""
say "DONE."
say "  summary (pasteable):  $SUMMARY"
say "  send this back:       $TARBALL  ($(du -h "$TARBALL" 2>/dev/null | cut -f1))"
