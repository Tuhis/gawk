#!/usr/bin/env python3
"""Tests for the coverage plumbing (`python3 -m unittest discover tools/coverage`).

The parsers are the part worth testing: they turn four vendor formats into the
number that gates every PR, and a silent misparse — counting hit blocks as
statements, say — would show up as a plausible-looking percentage rather than
an error. The partial-update rule in `badges` gets a test for the same reason:
its failure mode is five badges quietly resetting to whatever the last
single-component push measured.
"""

import json
import pathlib
import re
import sys
import tempfile
import unittest

import yaml

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import coverage  # noqa: E402


class TestParsers(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())

    def write(self, name: str, text: str) -> pathlib.Path:
        path = self.tmp / name
        path.write_text(text)
        return path

    def test_go_profile_sums_statements_not_blocks(self) -> None:
        # Three blocks, 2 + 5 + 3 statements; the 5-statement block is unhit.
        profile = self.write(
            "coverage.out",
            "mode: atomic\n"
            "github.com/tuhis/gawk/a.go:10.2,12.16 2 7\n"
            "github.com/tuhis/gawk/a.go:14.2,20.16 5 0\n"
            "github.com/tuhis/gawk/b.go:3.2,5.16 3 1\n",
        )
        self.assertEqual(coverage.parse_go(profile), (5, 10))

    def test_go_profile_rejects_a_malformed_line(self) -> None:
        profile = self.write("coverage.out", "mode: atomic\nnonsense\n")
        with self.assertRaises(SystemExit):
            coverage.parse_go(profile)

    def test_vitest_summary(self) -> None:
        summary = self.write(
            "coverage-summary.json",
            json.dumps({"total": {"lines": {"total": 200, "covered": 150, "pct": 75}}}),
        )
        self.assertEqual(coverage.parse_vitest(summary), (150, 200))

    def test_llvm_cov_summary(self) -> None:
        report = self.write(
            "llvm-cov.json",
            json.dumps({"data": [{"totals": {"lines": {"count": 400, "covered": 300}}}]}),
        )
        self.assertEqual(coverage.parse_llvm_cov(report), (300, 400))


class TestEmptyReportIsAnError(unittest.TestCase):
    """An empty report means the toolchain broke, not that coverage is 0%.

    The case that motivated it: `cargo llvm-cov` losing its instrumentation
    RUSTFLAGS to a target-specific override would produce a well-formed report
    measuring nothing, and a 0.0% badge would look like a coverage collapse
    rather than a broken job.
    """

    def test_normalise_refuses_a_report_with_no_units(self) -> None:
        tmp = pathlib.Path(tempfile.mkdtemp())
        report = tmp / "llvm-cov.json"
        report.write_text(json.dumps({"data": [{"totals": {"lines": {"count": 0, "covered": 0}}}]}))
        with self.assertRaises(SystemExit):
            coverage.main(
                [
                    "normalise",
                    "llvm-cov",
                    str(report),
                    "--component",
                    "gawk-broadcast-windows",
                    "--label",
                    "broadcast-windows",
                    "--out",
                    str(tmp / "out.json"),
                ]
            )


class TestRecordAndColor(unittest.TestCase):
    def test_percentage_is_rounded_to_one_decimal(self) -> None:
        self.assertEqual(coverage.record("c", "l", 1, 3, "statements")["pct"], 33.3)

    def test_empty_component_does_not_divide_by_zero(self) -> None:
        self.assertEqual(coverage.record("c", "l", 0, 0, "lines")["pct"], 0.0)

    def test_colors(self) -> None:
        self.assertEqual(coverage.color_for(90.0), "brightgreen")
        self.assertEqual(coverage.color_for(85.0), "brightgreen")
        self.assertEqual(coverage.color_for(70.0), "green")
        self.assertEqual(coverage.color_for(50.0), "yellow")
        self.assertEqual(coverage.color_for(49.9), "red")


class TestBadges(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.badges = self.tmp / "badges"

    def run_badges(self, *records: dict) -> dict:
        paths = []
        for i, rec in enumerate(records):
            path = self.tmp / f"rec{i}.json"
            path.write_text(json.dumps(rec))
            paths.append(str(path))
        coverage.main(["badges", str(self.badges), *paths])
        return json.loads((self.badges / "data.json").read_text())["components"]

    def test_a_skipped_component_keeps_its_previous_number(self) -> None:
        self.run_badges(
            coverage.record("gawk-server", "relay", 80, 100, "statements"),
            coverage.record("gawk-app", "app", 50, 100, "lines"),
        )
        # A second run carrying only gawk-app — the path-filtered case.
        components = self.run_badges(coverage.record("gawk-app", "app", 60, 100, "lines"))
        self.assertEqual(components["gawk-app"]["pct"], 60.0)
        self.assertEqual(components["gawk-server"]["pct"], 80.0)
        self.assertTrue((self.badges / "gawk-server.json").exists())

    def test_aggregate_is_weighted_by_size_not_a_mean_of_percentages(self) -> None:
        self.run_badges(
            coverage.record("gawk-server", "relay", 900, 1000, "statements"),
            coverage.record("gawk-app", "app", 0, 10, "lines"),
        )
        total = json.loads((self.badges / "total.json").read_text())
        # Weighted: 900/1010 = 89.1%. A mean of percentages would say 45%.
        self.assertEqual(total["message"], "89.1%")
        self.assertEqual(total["color"], "brightgreen")

    def test_badge_files_are_shields_endpoint_shaped(self) -> None:
        self.run_badges(coverage.record("gawk-server", "relay", 80, 100, "statements"))
        badge = json.loads((self.badges / "gawk-server.json").read_text())
        self.assertEqual(
            badge, {"schemaVersion": 1, "label": "relay", "message": "80.0%", "color": "green"}
        )


class TestFloorGate(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self._floors = coverage.FLOORS_FILE

    def tearDown(self) -> None:
        coverage.FLOORS_FILE = self._floors

    def floors(self, mapping: dict) -> None:
        path = self.tmp / "coverage-floors.json"
        path.write_text(json.dumps({"floors": mapping}))
        coverage.FLOORS_FILE = path

    def rec(self, component: str, pct_covered: int) -> str:
        path = self.tmp / f"{component}.json"
        path.write_text(json.dumps(coverage.record(component, "l", pct_covered, 100, "statements")))
        return str(path)

    def test_below_the_floor_fails(self) -> None:
        self.floors({"gawk-server": 70})
        self.assertEqual(coverage.main(["check", self.rec("gawk-server", 69)]), 1)

    def test_exactly_on_the_floor_passes(self) -> None:
        self.floors({"gawk-server": 70})
        self.assertEqual(coverage.main(["check", self.rec("gawk-server", 70)]), 0)

    def test_a_null_floor_reports_without_gating(self) -> None:
        self.floors({"gawk-server": None})
        self.assertEqual(coverage.main(["check", self.rec("gawk-server", 1)]), 0)

    def test_an_unlisted_component_is_an_error_not_a_pass(self) -> None:
        self.floors({"gawk-app": 70})
        with self.assertRaises(SystemExit):
            coverage.main(["check", self.rec("gawk-server", 99)])


class TestNoWorkflowCombinesRaceWithCoverage(unittest.TestCase):
    """D9's rule, enforced instead of remembered.

    Adding `-coverprofile` to an existing `-race` invocation is the obvious way
    to collect coverage and it cost this repository a CI outage: the two
    instrumentations compound, `gawk-server/wire` went 134s -> 574s, and the
    third run blew the 600s per-package timeout. Nothing about the combination
    looks wrong in review, and its symptom is a timeout in an unrelated-looking
    test — so the rule is checked here rather than trusted to whoever edits a
    workflow next.
    """

    def workflow_run_steps(self):
        workflows = pathlib.Path(coverage.REPO_ROOT, ".github", "workflows")
        for path in sorted(workflows.glob("*.yml")):
            spec = yaml.safe_load(path.read_text())
            for job_name, job in (spec.get("jobs") or {}).items():
                for step in job.get("steps") or []:
                    run = step.get("run")
                    if run:
                        yield path.name, job_name, run

    def test_no_go_test_asks_for_a_profile_under_the_race_detector(self) -> None:
        for filename, job, run in self.workflow_run_steps():
            for line in run.splitlines():
                if "go test" not in line or "-race" not in line:
                    continue
                if "-coverprofile" in line or re.search(r"-cover(\s|$)", line):
                    self.fail(
                        f"{filename}:{job} runs `go test` with -race AND coverage:\n"
                        f"    {line.strip()}\n"
                        "Split them into two runs — see docs/43 D9. (`go test -cover`\n"
                        "with -race silently selects -covermode=atomic, which is the\n"
                        "expensive half of the combination.)"
                    )


class TestFloorsFileItself(unittest.TestCase):
    def test_every_component_that_reports_has_a_floor_entry(self) -> None:
        floors = json.loads(coverage.FLOORS_FILE.read_text())["floors"]
        self.assertEqual(
            sorted(floors),
            [
                "gawk-admin",
                "gawk-admin-ui",
                "gawk-app",
                "gawk-broadcast",
                "gawk-broadcast-windows",
                "gawk-server",
                "gawk-telemetry",
                "gawk-telemetry-ui",
            ],
        )


if __name__ == "__main__":
    unittest.main()
