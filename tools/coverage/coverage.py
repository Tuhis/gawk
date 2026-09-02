#!/usr/bin/env python3
"""Normalise, gate and publish test-coverage numbers (R41, docs/43).

Six components in four toolchains report coverage in four different formats.
Rather than teach every CI job to parse its own, each job runs one `normalise`
subcommand and gets the same small record out:

    {"component": "gawk-server", "label": "relay",
     "covered": 3218, "total": 4011, "pct": 80.2, "unit": "statements"}

Counts, not just a percentage, because the aggregate badge is a weighted mean
over the whole tree — averaging six percentages would let a 40-line UI bundle
outvote the relay.

Subcommands:

  normalise go       <profile>  --component --label   Go `-coverprofile` output
  normalise vitest   <summary>  --component --label   vitest json-summary
  normalise llvm-cov <json>     --component --label   `cargo llvm-cov --json`
  check     <record...>                               fail below the floor
  badges    <badges-dir> <record...>                  update the badges branch

`check` is the gate and runs in the component's own job, so a shortfall is
reported by the job that owns the code. `badges` runs only on pushes to main.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys
from typing import Any

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
FLOORS_FILE = REPO_ROOT / "coverage-floors.json"

# Shields renders the endpoint response verbatim, so the thresholds live here
# rather than in a URL query. Deliberately coarse: a badge that changes colour
# on a one-statement move trains people to ignore it.
COLORS = ((85.0, "brightgreen"), (70.0, "green"), (50.0, "yellow"), (0.0, "red"))


def color_for(pct: float) -> str:
    for threshold, color in COLORS:
        if pct >= threshold:
            return color
    return "red"


def record(component: str, label: str, covered: int, total: int, unit: str) -> dict[str, Any]:
    pct = round(100.0 * covered / total, 1) if total else 0.0
    return {
        "component": component,
        "label": label,
        "covered": covered,
        "total": total,
        "pct": pct,
        "unit": unit,
    }


# --- normalisers ------------------------------------------------------------


def parse_go(path: pathlib.Path) -> tuple[int, int]:
    """Sum statement counts from a Go coverage profile.

    Every line after the `mode:` header is
    `file.go:startLine.col,endLine.col numStatements hitCount`, so the totals
    here are the same ones `go tool cover -func` prints — computed without
    shelling out to a toolchain the publishing job does not otherwise need.
    """
    covered = total = 0
    for line in path.read_text().splitlines():
        if not line or line.startswith("mode:"):
            continue
        fields = line.split()
        if len(fields) != 3:
            raise SystemExit(f"{path}: cannot parse coverage line: {line!r}")
        statements, hits = int(fields[1]), int(fields[2])
        total += statements
        if hits > 0:
            covered += statements
    return covered, total


def parse_vitest(path: pathlib.Path) -> tuple[int, int]:
    """Lines covered/total from vitest's `json-summary` reporter.

    Lines rather than statements: for TSX the two barely differ, and lines are
    what the reporter's own text output shows, so the badge and a local
    `npm run test:coverage` agree.
    """
    data = json.loads(path.read_text())
    lines = data["total"]["lines"]
    return int(lines["covered"]), int(lines["total"])


def parse_llvm_cov(path: pathlib.Path) -> tuple[int, int]:
    """Lines covered/total from `cargo llvm-cov --json --summary-only`.

    llvm-cov also reports regions and functions; lines keep the Rust number
    comparable with the vitest one, which matters because they land in the
    same weighted aggregate.
    """
    data = json.loads(path.read_text())
    totals = data["data"][0]["totals"]["lines"]
    return int(totals["covered"]), int(totals["count"])


PARSERS = {"go": parse_go, "vitest": parse_vitest, "llvm-cov": parse_llvm_cov}
UNITS = {"go": "statements", "vitest": "lines", "llvm-cov": "lines"}


# --- the floor gate ---------------------------------------------------------


def load_floors() -> dict[str, Any]:
    floors = json.loads(FLOORS_FILE.read_text())
    return floors.get("floors", {})


def cmd_check(args: argparse.Namespace) -> int:
    floors = load_floors()
    failed = False
    for path in args.records:
        rec = json.loads(pathlib.Path(path).read_text())
        component = rec["component"]
        if component not in floors:
            raise SystemExit(
                f"::error::{component} has no entry in coverage-floors.json — "
                "add one (see docs/43-coverage-reporting.md)"
            )
        floor = floors[component]
        summary = f"{component}: {rec['pct']}% ({rec['covered']}/{rec['total']} {rec['unit']})"
        # A null floor is the documented escape hatch for a component whose
        # baseline has not been measured yet — report, do not gate.
        if floor is None:
            print(f"{summary}, floor not set")
            continue
        if rec["pct"] + 1e-9 < floor:
            print(f"::error::{summary} is below its floor of {floor}%")
            failed = True
        else:
            print(f"{summary}, floor {floor}%")
    return 1 if failed else 0


# --- the badges branch ------------------------------------------------------


def cmd_badges(args: argparse.Namespace) -> int:
    """Merge new records into the badges branch and regenerate every badge.

    Partial by construction. `ci.yml` is path-filtered, so a push that only
    touches gawk-app runs one coverage job — and the five components that did
    not run must keep the numbers they last measured, which are still true of
    code that did not move. Only components named in `records` are replaced;
    `data.json` carries the rest forward.
    """
    badges_dir = pathlib.Path(args.badges_dir)
    badges_dir.mkdir(parents=True, exist_ok=True)
    data_file = badges_dir / "data.json"

    data = json.loads(data_file.read_text()) if data_file.exists() else {}
    components: dict[str, Any] = data.get("components", {})

    for path in args.records:
        rec = json.loads(pathlib.Path(path).read_text())
        components[rec["component"]] = rec

    # Stable key order keeps the diff on the badges branch readable and stops
    # a re-run with identical numbers from producing a commit.
    components = dict(sorted(components.items()))

    covered = sum(c["covered"] for c in components.values())
    total = sum(c["total"] for c in components.values())
    aggregate = record("total", "coverage", covered, total, "units")

    data_file.write_text(json.dumps({"components": components}, indent=2, sort_keys=True) + "\n")

    for name, rec in list(components.items()) + [("total", aggregate)]:
        badge = {
            "schemaVersion": 1,
            "label": rec["label"],
            "message": f"{rec['pct']}%",
            "color": color_for(rec["pct"]),
        }
        (badges_dir / f"{name}.json").write_text(json.dumps(badge, indent=2) + "\n")

    print(f"aggregate: {aggregate['pct']}% ({covered}/{total} units across {len(components)})")
    return 0


# --- entry point ------------------------------------------------------------


def cmd_normalise(args: argparse.Namespace) -> int:
    covered, total = PARSERS[args.format](pathlib.Path(args.input))
    # A report that measured nothing is a broken toolchain, not a component
    # with no coverage — an uninstrumented `cargo llvm-cov` run, a vitest
    # `include` that matches no file, a profile from a build that failed. All
    # three would otherwise publish a plausible 0.0% badge and pass any floor
    # that has not been set yet.
    if total == 0:
        raise SystemExit(
            f"::error::{args.component}: {args.input} reports 0 measurable "
            f"{UNITS[args.format]} — the report is empty, not the coverage"
        )
    rec = record(args.component, args.label, covered, total, UNITS[args.format])
    pathlib.Path(args.out).write_text(json.dumps(rec, indent=2) + "\n")
    print(f"{rec['component']}: {rec['pct']}% ({covered}/{total} {rec['unit']})")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)

    norm = sub.add_parser("normalise", help="turn a tool's report into one record")
    norm.add_argument("format", choices=sorted(PARSERS))
    norm.add_argument("input")
    norm.add_argument("--component", required=True)
    norm.add_argument("--label", required=True)
    norm.add_argument("--out", required=True)
    norm.set_defaults(func=cmd_normalise)

    check = sub.add_parser("check", help="fail if a record is below its floor")
    check.add_argument("records", nargs="+")
    check.set_defaults(func=cmd_check)

    badges = sub.add_parser("badges", help="regenerate the badges branch")
    badges.add_argument("badges_dir")
    badges.add_argument("records", nargs="*")
    badges.set_defaults(func=cmd_badges)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
