#!/usr/bin/env python3
"""Regenerate the per-component THIRD-PARTY-NOTICES.md files.

Apache-2.0 lets us license our own code freely, but every third-party
dependency we redistribute keeps its own terms, and the permissive ones
(MIT, BSD, ISC, Apache-2.0) all require the copyright notice and the
licence text to travel with the code. That obligation attaches to the
*binaries and images we publish*, so what each file lists is what actually
gets linked into the shipped artifact — not what appears in a manifest:

  * Go        `go list -deps` over the real build (plus the build tags the
              shipped image uses). This is deliberately NOT the go.mod
              require list: gawk-telemetry's go.mod names arrow-go and
              flatbuffers, and its binary imports neither.
  * Rust      `cargo tree -e normal --target x86_64-pc-windows-msvc`. `-e
              normal` drops build- and dev-dependencies (proc macros run at
              compile time and are not in the EXE); the target filter drops
              the Linux/macOS halves of every cfg-gated crate.
  * npm       the lockfile minus `dev: true` — i.e. what Vite puts in the
              bundle. lightningcss and friends are build tools that never
              reach a browser.

Regenerating needs the network and a populated module/crate/npm cache; it
is a release-time chore, not a CI gate. CI enforces the weaker, cheap
property instead — that every licence stays on the allowlist (see the
`licenses` job in .github/workflows/ci.yml and deny.toml). The two are
complementary: the gate catches a copyleft dependency arriving, this
script keeps the attribution accurate.

    python3 tools/licenses/gen-notices.py            # regenerate everything
    python3 tools/licenses/gen-notices.py server app # regenerate some of it
    python3 tools/licenses/gen-notices.py --check    # CI gate, see below

`--check` is the cheap half, and the half CI runs: it asserts that every
Go and npm dependency's license is on ALLOWED_LICENSES and exits non-zero
otherwise. It reads declared metadata (the npm lockfile) and the module
cache (Go) — no npm install, no notices rewrite. Rust is covered by
cargo-deny against `deny.toml`, which is the ecosystem's own tool and
also understands the SPDX *expressions* Cargo manifests use.

Licence *texts* are deduplicated after stripping copyright lines, so the
MIT block appears once with every copyright holder listed above it rather
than sixty near-identical copies. The per-package copyright lines are the
part the licences actually require; the boilerplate is identical by
construction.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

LICENSE_FILE_RE = re.compile(
    r"^(LICEN[SC]E|COPYING|COPYRIGHT|NOTICE|UNLICENSE)([-._].*)?$", re.IGNORECASE
)
COPYRIGHT_RE = re.compile(r"^\s*(?:#\s*)?copyright\s", re.IGNORECASE)
# A real attribution names a year or carries a (c)/© mark. Without this, the
# BSD boilerplate's own "copyright notice, this list of conditions..." clause
# gets scraped as though it were a copyright holder.
COPYRIGHT_EVIDENCE_RE = re.compile(r"(\b(19|20)\d{2}\b|\(c\)|©)", re.IGNORECASE)

# Ordered: the first pattern that matches wins, so the narrower tests for the
# copyleft families come before the permissive ones they textually resemble.
CLASSIFIERS = [
    ("AGPL-3.0", "GNU AFFERO GENERAL PUBLIC LICENSE"),
    ("LGPL", "GNU LESSER GENERAL PUBLIC LICENSE"),
    ("GPL", "GNU GENERAL PUBLIC LICENSE"),
    ("MPL-2.0", "Mozilla Public License"),
    ("Apache-2.0", "Apache License"),
    ("Unlicense", "released into the public domain"),
    ("ISC", "Permission to use, copy, modify, and/or distribute"),
    ("MIT", "Permission is hereby granted, free of charge"),
    ("BSD", "Redistribution and use in source and binary forms"),
    ("Zlib", "altered source versions must be plainly marked"),
    ("BSL-1.0", "Boost Software License"),
]
# Families where "this text also contains that text" is a containment, not a
# dual offer: an LGPL file quotes the GPL, and saying "LGPL OR GPL" would
# misrepresent it. For these, the first match is the whole answer.
COPYLEFT = {"AGPL-3.0", "LGPL", "GPL", "MPL-2.0"}


def run(cmd: list[str], cwd: Path, env: dict | None = None) -> str:
    full_env = {**os.environ, **(env or {})}
    proc = subprocess.run(
        cmd, cwd=cwd, env=full_env, capture_output=True, text=True, check=False
    )
    if proc.returncode != 0:
        sys.exit(f"! {' '.join(cmd)} (in {cwd}) failed:\n{proc.stderr[-2000:]}")
    return proc.stdout


def classify(text: str) -> str:
    """Best-effort SPDX label for a licence text.

    A single file can genuinely offer two licences — gio ships one LICENSE
    holding both the Unlicense and MIT texts — so permissive matches are
    joined rather than resolved to the first hit.
    """
    lowered = text.lower()
    matches = [spdx for spdx, needle in CLASSIFIERS if needle.lower() in lowered]
    if not matches:
        return "UNKNOWN"
    for spdx in matches:
        if spdx in COPYLEFT:
            return spdx
    if "BSD" in matches:
        # The clause count is the whole distinction, and it is the one thing a
        # reader checks: 3-clause adds the no-endorsement clause.
        matches[matches.index("BSD")] = (
            "BSD-3-Clause" if "neither the name" in lowered else "BSD-2-Clause"
        )
    return " OR ".join(dict.fromkeys(matches))


def read_license_files(root: Path) -> list[tuple[str, str]]:
    """Every licence-ish file at the root of a package, as (filename, text)."""
    if not root or not root.is_dir():
        return []
    out = []
    for entry in sorted(root.iterdir()):
        if entry.is_file() and LICENSE_FILE_RE.match(entry.name):
            try:
                text = entry.read_text(encoding="utf-8", errors="replace").strip()
            except OSError:
                continue
            if text:
                out.append((entry.name, text))
    # Some crates keep them in a LICENSES/ directory (the REUSE convention).
    licenses_dir = root / "LICENSES"
    if licenses_dir.is_dir():
        for entry in sorted(licenses_dir.iterdir()):
            if entry.is_file():
                text = entry.read_text(encoding="utf-8", errors="replace").strip()
                if text:
                    out.append((f"LICENSES/{entry.name}", text))
    return out


# A copyright line inside a licence *document* belongs to whoever wrote the
# licence, not to the package shipping it. Every GPL/LGPL/AGPL text opens with
# the FSF's copyright over the document itself, which would otherwise be
# scraped and printed as if the FSF authored the dependency.
LICENSE_DOC_HOLDERS = ("free software foundation",)

# Where to look for a real copyright when the licence files yield none —
# the REUSE convention puts it in a header comment on the source instead.
SOURCE_HINTS = (
    "src/lib.rs",
    "src/main.rs",
    "lib.rs",
    "main.rs",
    "doc.go",  # the Go convention for a package-level header comment
    "README.md",
)


def copyright_lines(texts: list[tuple[str, str]]) -> list[str]:
    seen, out = set(), []
    for _, text in texts:
        for line in text.splitlines():
            if COPYRIGHT_RE.match(line):
                clean = " ".join(line.split()).rstrip(".")
                # "Copyright (c) [year] [name]" / "Copyright (C) <year> <name
                # of author>" placeholders in a bundled licence *template* are
                # noise, not an attribution — and neither is the BSD clause
                # that happens to begin with the word "copyright".
                if "[" in clean or "<year>" in clean.lower():
                    continue
                if not COPYRIGHT_EVIDENCE_RE.search(clean):
                    continue
                if any(h in clean.lower() for h in LICENSE_DOC_HOLDERS):
                    continue
                if clean not in seen:
                    seen.add(clean)
                    out.append(clean)
    return out


def copyright_from_sources(root: Path | None) -> list[str]:
    """Fall back to a REUSE-style header when the licence files carry none.

    Slint is the case that forces this: its crates ship licence *texts* in
    LICENSES/ and put `// Copyright © SixtyFPS GmbH` on every source file.
    """
    if not root or not root.is_dir():
        return []
    for hint in SOURCE_HINTS:
        candidate = root / hint
        if not candidate.is_file():
            continue
        try:
            head = candidate.read_text(encoding="utf-8", errors="replace")[:4000]
        except OSError:
            continue
        for line in head.splitlines()[:40]:
            stripped = line.lstrip("/#*<!- \t")
            if COPYRIGHT_RE.match(stripped):
                clean = " ".join(stripped.split()).rstrip(".")
                if "[" in clean or "<year>" in clean.lower():
                    continue
                if any(h in clean.lower() for h in LICENSE_DOC_HOLDERS):
                    continue
                # A bare "Copyright (c) 2019" with no holder helps nobody.
                if COPYRIGHT_EVIDENCE_RE.search(clean) and len(clean.split()) > 2:
                    return [clean]
    return []


def normalize(text: str) -> str:
    """Licence text with the parts that vary per package removed.

    Two MIT licences differ only in their copyright line; collapsing that
    away is what lets the appendix carry one MIT block instead of sixty.
    """
    lines = [ln for ln in text.splitlines() if not COPYRIGHT_RE.match(ln)]
    return " ".join(" ".join(lines).split()).lower()


class Package:
    def __init__(self, name: str, version: str, declared: str | None, root: Path | None):
        self.name = name
        self.version = version
        self.files = read_license_files(root) if root else []
        self.copyrights = copyright_lines(self.files) or copyright_from_sources(root)
        self.root = root
        if declared:
            self.spdx = declared
        elif self.files:
            self.spdx = " OR ".join(
                dict.fromkeys(classify(text) for _, text in self.files)
            )
        else:
            self.spdx = "UNKNOWN"

    @property
    def key(self) -> tuple[str, str]:
        return (self.name.lower(), self.version)


# --------------------------------------------------------------------------
# Ecosystems
# --------------------------------------------------------------------------


def collect_go(module: str, tags: str = "") -> list[Package]:
    cwd = REPO / module
    fmt = "{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}{{end}}"
    cmd = ["go", "list", "-deps"]
    if tags:
        cmd += ["-tags", tags]
    cmd += ["-f", fmt, "./..."]
    seen: dict[tuple[str, str], Package] = {}
    for line in run(cmd, cwd).splitlines():
        if not line.strip():
            continue
        path, version, directory = (line.split("\t") + ["", ""])[:3]
        if path.startswith("github.com/Tuhis/gawk/"):
            continue  # first-party; covered by the repo LICENSE
        pkg = Package(path, version, None, Path(directory) if directory else None)
        seen.setdefault(pkg.key, pkg)
    return sorted(seen.values(), key=lambda p: p.name.lower())


def collect_cargo(workspace: str, target: str) -> list[Package]:
    cwd = REPO / workspace
    meta = json.loads(run(["cargo", "metadata", "--format-version", "1"], cwd))
    declared, roots = {}, {}
    for pkg in meta["packages"]:
        declared[(pkg["name"].lower(), pkg["version"])] = pkg.get("license")
        roots[(pkg["name"].lower(), pkg["version"])] = Path(pkg["manifest_path"]).parent
    workspace_members = {
        m.split()[0].lower() for m in meta["workspace_members"] if " " in m
    } | {
        # Cargo's member ids are opaque URLs on newer versions; fall back to
        # the package list filtered by source == null (i.e. path packages).
        p["name"].lower()
        for p in meta["packages"]
        if p.get("source") is None
    }

    tree = run(
        [
            "cargo", "tree",
            "-e", "normal",
            "--target", target,
            "--prefix", "none",
            "--no-dedupe",
        ],
        cwd,
    )
    seen: dict[tuple[str, str], Package] = {}
    for line in tree.splitlines():
        parts = line.strip().split()
        if len(parts) < 2 or not parts[1].startswith("v"):
            continue
        name, version = parts[0], parts[1][1:]
        if name.lower() in workspace_members and "(proc-macro)" not in line:
            # First-party crates AND the vendored path deps: the vendored ones
            # are re-added below with their real upstream licences.
            if not (REPO / workspace / "vendor" / name).is_dir():
                continue
        key = (name.lower(), version)
        if key in seen:
            continue
        seen[key] = Package(name, version, declared.get(key), roots.get(key))
    return sorted(seen.values(), key=lambda p: p.name.lower())


def collect_npm(project: str) -> list[Package]:
    cwd = REPO / project
    lock = json.loads((cwd / "package-lock.json").read_text(encoding="utf-8"))
    runtime = {
        path: entry
        for path, entry in lock["packages"].items()
        if path and not entry.get("dev")
    }
    # Licence *texts* only exist on disk, so install exactly the runtime tree.
    had_node_modules = (cwd / "node_modules").is_dir()
    run(["npm", "ci", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund"], cwd)
    out = []
    for path, entry in sorted(runtime.items()):
        name = re.sub(r"^.*node_modules/", "", path)
        root = cwd / path
        out.append(Package(name, entry.get("version", ""), entry.get("license"), root))
    if had_node_modules:
        # Put back what was there: a working tree that could `npm run build`
        # before this script ran must still be able to afterwards. On a fresh
        # clone (CI, and Renovate's postUpgradeTasks) there was nothing to
        # restore, and a second full install is a minute of nothing.
        run(["npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"], cwd)
    return sorted(out, key=lambda p: p.name.lower())


# --------------------------------------------------------------------------
# Rendering
# --------------------------------------------------------------------------


def render(component: str, blurb: str, sources: str, packages: list[Package]) -> str:
    lines = [
        f"# Third-party notices — `{component}`",
        "",
        blurb,
        "",
        "This file is generated. Do not edit it by hand — run",
        "`python3 tools/licenses/gen-notices.py` and commit the result. See that",
        "script for what counts as a dependency here and why.",
        "",
        f"**Scope:** {sources}",
        "",
    ]

    counts: dict[str, int] = {}
    for pkg in packages:
        counts[pkg.spdx] = counts.get(pkg.spdx, 0) + 1
    lines += [
        f"## Summary — {len(packages)} package"
        + ("s" if len(packages) != 1 else ""),
        "",
        "| License (as declared) | Packages |",
        "|---|---:|",
    ]
    for spdx, count in sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])):
        lines.append(f"| `{spdx}` | {count} |")
    lines.append("")

    if not packages:
        lines += [
            "This component links no third-party code. Its dependencies are the Go",
            "standard library and other modules in this repository, all covered by",
            "the repository [LICENSE](../LICENSE).",
            "",
        ]
        return "\n".join(lines)

    lines += ["## Packages", "", "| Package | Version | License | Copyright |", "|---|---|---|---|"]
    for pkg in packages:
        holders = "<br>".join(pkg.copyrights) if pkg.copyrights else "—"
        version = pkg.version or "—"
        lines.append(f"| `{pkg.name}` | {version} | `{pkg.spdx}` | {holders} |")
    lines.append("")

    # Group the distinct licence texts. Identical boilerplate collapses; the
    # copyright lines that differ are already in the table above.
    groups: dict[str, dict] = {}
    for pkg in packages:
        for filename, text in pkg.files:
            norm = normalize(text)
            if not norm:
                continue
            group = groups.setdefault(
                norm, {"text": text, "spdx": classify(text), "packages": []}
            )
            if pkg.name not in group["packages"]:
                group["packages"].append(pkg.name)
            # Prefer the shortest verbatim copy as the representative: they are
            # equal modulo copyright lines, and the shortest has least cruft.
            if len(text) < len(group["text"]):
                group["text"] = text

    lines += [
        "## License texts",
        "",
        "Each distinct license text below appears once. Texts that differ only in",
        "their copyright line are shown once, with every holder listed in the table",
        "above — that line is the part the license requires be retained, and the",
        "boilerplate around it is identical by construction.",
        "",
    ]
    ordered = sorted(
        groups.values(), key=lambda g: (g["spdx"], -len(g["packages"]))
    )
    for index, group in enumerate(ordered, start=1):
        shown = ", ".join(f"`{n}`" for n in sorted(group["packages"])[:12])
        if len(group["packages"]) > 12:
            shown += f" — and {len(group['packages']) - 12} more"
        lines += [
            f"### {index}. {group['spdx']} — {len(group['packages'])} package"
            + ("s" if len(group["packages"]) != 1 else ""),
            "",
            f"Applies to: {shown}",
            "",
            "```",
            group["text"].replace("```", "'''"),
            "```",
            "",
        ]
    return "\n".join(lines)


COMPONENTS = {
    "server": dict(
        path="gawk-server",
        blurb=(
            "The relay ships as a static binary in a distroless image. Everything\n"
            "listed here is compiled into `gawk-server` (and `gawk-echo`)."
        ),
        sources="`go list -deps ./...` in `gawk-server/`.",
        collect=lambda: collect_go("gawk-server"),
    ),
    "app": dict(
        path="gawk-app",
        blurb=(
            "The viewer/broadcaster SPA. Only packages that reach the browser are\n"
            "listed: the build toolchain (Vite, TypeScript, oxlint, lightningcss)\n"
            "runs on the build machine and ships nothing into the bundle."
        ),
        sources="`package-lock.json` entries without `dev: true`.",
        collect=lambda: collect_npm("gawk-app"),
    ),
    "broadcast": dict(
        path="gawk-broadcast",
        blurb=(
            "The native Linux broadcaster (`gawk-broadcast`, `gawk-broadcast-gui`,\n"
            "`gawk-pw-helper`). GStreamer is deliberately absent from this list: the\n"
            "broadcaster runs `gst-launch-1.0` as a separate process installed by the\n"
            "user's distribution, so no GStreamer code is linked or redistributed\n"
            "here. `gawk-pw-helper` does dynamically link `libpipewire-0.3` (MIT),\n"
            "which the user likewise supplies."
        ),
        sources="`go list -deps ./...` in `gawk-broadcast/`.",
        collect=lambda: collect_go("gawk-broadcast"),
    ),
    "telemetry": dict(
        path="gawk-telemetry",
        blurb=(
            "The optional diagnostics service, listed as the *deployed* image builds\n"
            "it: `-tags duckdb`. A default (tag-free) build links no third-party Go\n"
            "code at all. The bundled dashboard UI is covered by\n"
            "`ui/THIRD-PARTY-NOTICES.md`."
        ),
        sources="`go list -deps -tags duckdb ./...` in `gawk-telemetry/`.",
        collect=lambda: collect_go("gawk-telemetry", tags="duckdb"),
    ),
    "telemetry-ui": dict(
        path="gawk-telemetry/ui",
        blurb=(
            "The telemetry dashboard SPA, embedded into the `gawk-telemetry` binary.\n"
            "Build-time-only packages are excluded, as for `gawk-app`."
        ),
        sources="`package-lock.json` entries without `dev: true`.",
        collect=lambda: collect_npm("gawk-telemetry/ui"),
    ),
    "admin-ui": dict(
        path="gawk-admin/ui",
        blurb=(
            "The moderation portal SPA, embedded into the `gawk-admin` binary.\n"
            "Build-time-only packages are excluded, as for `gawk-app`. The OIDC\n"
            "public-client flow is hand-rolled against WebCrypto (docs/42 §4.8),\n"
            "so no OIDC library appears here."
        ),
        sources="`package-lock.json` entries without `dev: true`.",
        collect=lambda: collect_npm("gawk-admin/ui"),
    ),
    "windows": dict(
        path="gawk-broadcast-windows",
        blurb=(
            "The native Windows broadcaster, a single static `gawk-broadcast.exe`.\n"
            "\n"
            "Two entries deserve a second look. **Slint** and its `i-slint-*` crates\n"
            "are tri-licensed; gawk uses them under the **Slint Royalty-free License\n"
            "version 2.0**, whose attribution condition is met by the \"Made with\n"
            "Slint\" badge on the project README and release pages. **libopus** is\n"
            "compiled in statically through `audiopus_sys`; the ISC license below\n"
            "covers the Rust bindings, and Xiph's BSD-3-Clause license covers the C\n"
            "library itself.\n"
            "\n"
            "The MSVC C runtime the binary links against is Microsoft's and is\n"
            "governed by the Visual Studio license terms, not by anything here."
        ),
        sources=(
            "`cargo tree -e normal --target x86_64-pc-windows-msvc` — build- and "
            "dev-dependencies (proc macros, test harnesses) are excluded because "
            "they are not part of the shipped executable."
        ),
        collect=lambda: collect_cargo(
            "gawk-broadcast-windows", "x86_64-pc-windows-msvc"
        ),
    ),
}


# Everything gawk is willing to redistribute: permissive, notice-only terms
# that impose no obligation on a downstream user beyond keeping the notice.
# Adding to this list is a licensing decision, not a build fix — if a
# dependency lands here that is not on it, either drop the dependency or
# decide, deliberately, that gawk now carries that obligation. The Rust
# equivalent lives in deny.toml; keep the two in step.
ALLOWED_LICENSES = {
    "0BSD",
    "Apache-2.0",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "BlueOak-1.0.0",
    "CC0-1.0",
    "ISC",
    "MIT",
    "MIT-0",
    "Unlicense",
    "Zlib",
}


def split_expression(expression: str) -> list[list[str]]:
    """SPDX expression -> alternatives, each a list of required licenses.

    Deliberately small: it handles the `A OR B` and `A AND B` shapes that
    appear in practice, including the non-standard `MIT/Apache-2.0` spelling
    Cargo manifests still carry. Anything more exotic is left as one opaque
    token, which fails the allowlist and gets a human's attention — the safe
    direction.
    """
    cleaned = expression.replace("(", " ").replace(")", " ").replace("/", " OR ")
    return [
        [term.strip() for term in alternative.split(" AND ") if term.strip()]
        for alternative in re.split(r"\s+OR\s+", cleaned)
        if alternative.strip()
    ]


def expression_allowed(expression: str) -> bool:
    return any(
        all(term in ALLOWED_LICENSES for term in alternative)
        for alternative in split_expression(expression)
    )


def check() -> int:
    """Assert every Go and npm dependency is on the allowlist."""
    failures: list[str] = []
    checked = 0

    for module, tags in (
        ("gawk-server", ""),
        ("gawk-broadcast", ""),
        ("gawk-telemetry", "duckdb"),
    ):
        for pkg in collect_go(module, tags):
            checked += 1
            if not expression_allowed(pkg.spdx):
                failures.append(f"{module}: {pkg.name}@{pkg.version} is {pkg.spdx}")

    for project in ("gawk-app", "gawk-telemetry/ui", "gawk-admin/ui"):
        lock = json.loads(
            (REPO / project / "package-lock.json").read_text(encoding="utf-8")
        )
        for path, entry in lock["packages"].items():
            if not path or entry.get("dev"):
                continue  # build tooling is not redistributed
            checked += 1
            declared = entry.get("license")
            name = re.sub(r"^.*node_modules/", "", path)
            if not declared:
                failures.append(f"{project}: {name} declares no license")
            elif not expression_allowed(declared):
                failures.append(f"{project}: {name} is {declared}")

    if failures:
        print(f"{len(failures)} dependency license(s) not on the allowlist:\n")
        for failure in failures:
            print(f"  - {failure}")
        print(
            "\nAllowed: " + ", ".join(sorted(ALLOWED_LICENSES))
            + "\nSee tools/licenses/gen-notices.py — adding to that list is a"
            " licensing decision."
        )
        return 1
    print(f"OK — {checked} Go and npm dependencies, all on the allowlist.")
    return 0


def main() -> None:
    if "--check" in sys.argv[1:]:
        sys.exit(check())
    wanted = sys.argv[1:] or list(COMPONENTS)
    unknown = [name for name in wanted if name not in COMPONENTS]
    if unknown:
        sys.exit(f"unknown component(s): {', '.join(unknown)}\nknown: {', '.join(COMPONENTS)}")
    for name in wanted:
        spec = COMPONENTS[name]
        print(f"==> {name}", flush=True)
        packages = spec["collect"]()
        out = REPO / spec["path"] / "THIRD-PARTY-NOTICES.md"
        out.write_text(
            render(Path(spec["path"]).name, spec["blurb"], spec["sources"], packages)
            + "\n",
            encoding="utf-8",
        )
        print(f"    {len(packages)} packages -> {out.relative_to(REPO)}")


if __name__ == "__main__":
    main()
