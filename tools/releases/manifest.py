#!/usr/bin/env python3
"""Per-component release manifest (R46, docs/46).

One tiny JSON file per native component, published to the orphan `badges`
branch as `releases/<component>/latest.json` by the job that attaches the
release assets. It is the "what is the newest gawk-broadcast?" answer for
anything that must not use the GitHub API: the project site's Download
section (R46) and, later, the desktop apps' own update check (R45).

Two subcommands:

  build     write the manifest for a release from the asset directory the
            attach job just uploaded — the same files, so the checksums are
            those of the bytes on the release page, not a recomputation.
  validate  check a manifest has the shape consumers rely on; the writer
            runs it before pushing, and the tests pin the shape.

No third-party imports: this runs inside a composite action on a runner
that has Python and nothing else installed for it.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path

SCHEMA = 1
REPO = "Tuhis/gawk"

# The release part of a version — what release-please writes into the
# manifest. Prereleases and build metadata are not something the release
# pipeline produces today, so they are rejected rather than half-supported.
_VERSION = re.compile(r"^\d+\.\d+\.\d+$")
_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_TIMESTAMP = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")


class ManifestError(ValueError):
    pass


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def build(component: str, version: str, tag: str, published_at: str,
          asset_dir: Path, primary: str, repo: str = REPO) -> dict:
    """Assemble the manifest. Every asset in `asset_dir` is listed; `primary`
    names the one a download button points at (the tarball on Linux, the EXE
    on Windows). The checksum file itself is listed too — a consumer that
    wants to verify the way INSTALL.md says can find it by name."""
    if not asset_dir.is_dir():
        raise ManifestError(f"{asset_dir} is not a directory")
    files = sorted(p for p in asset_dir.iterdir() if p.is_file())
    if not files:
        raise ManifestError(f"{asset_dir} holds no files")
    names = {p.name for p in files}
    if primary not in names:
        raise ManifestError(f"primary asset {primary!r} is not in {asset_dir}: {sorted(names)}")

    base = f"https://github.com/{repo}/releases/download/{tag}"
    assets = {
        p.name: {"size": p.stat().st_size, "sha256": _sha256(p), "url": f"{base}/{p.name}"}
        for p in files
    }
    manifest = {
        "schema": SCHEMA,
        "component": component,
        "version": version,
        "tag": tag,
        "published_at": published_at,
        "release_url": f"https://github.com/{repo}/releases/tag/{tag}",
        "asset": {"name": primary, **assets[primary]},
        "assets": assets,
    }
    validate(manifest)
    return manifest


def validate(manifest: object) -> None:
    """Raise ManifestError unless `manifest` is exactly the shape consumers
    read. Deliberately strict about the fields the site and the update check
    key on, and silent about extra keys — a later schema may add some."""
    if not isinstance(manifest, dict):
        raise ManifestError("manifest is not an object")
    if manifest.get("schema") != SCHEMA:
        raise ManifestError(f"schema must be {SCHEMA}, got {manifest.get('schema')!r}")
    for key in ("component", "version", "tag", "published_at", "release_url"):
        if not isinstance(manifest.get(key), str) or not manifest[key]:
            raise ManifestError(f"{key} must be a non-empty string")
    if not _VERSION.match(manifest["version"]):
        raise ManifestError(f"version {manifest['version']!r} is not X.Y.Z")
    # Both tag spellings: `component/vX.Y.Z` since the tag-separator change,
    # `component-vX.Y.Z` on every release cut before it — and a backfill of
    # one of those is exactly when the attach job runs this on a legacy tag.
    stem = f"{manifest['component']}"
    if manifest["tag"] not in (f"{stem}/v{manifest['version']}", f"{stem}-v{manifest['version']}"):
        raise ManifestError(
            f"tag {manifest['tag']!r} does not match {stem}/v{manifest['version']}")
    if not _TIMESTAMP.match(manifest["published_at"]):
        raise ManifestError(f"published_at {manifest['published_at']!r} is not an RFC 3339 UTC timestamp")

    assets = manifest.get("assets")
    if not isinstance(assets, dict) or not assets:
        raise ManifestError("assets must be a non-empty object")
    for name, a in assets.items():
        if not isinstance(a, dict):
            raise ManifestError(f"assets[{name!r}] is not an object")
        if not isinstance(a.get("size"), int) or a["size"] <= 0:
            raise ManifestError(f"assets[{name!r}].size must be a positive integer")
        if not isinstance(a.get("sha256"), str) or not _SHA256.match(a["sha256"]):
            raise ManifestError(f"assets[{name!r}].sha256 must be 64 hex characters")
        if not isinstance(a.get("url"), str) or not a["url"].endswith(f"/{name}"):
            raise ManifestError(f"assets[{name!r}].url must end in /{name}")

    primary = manifest.get("asset")
    if not isinstance(primary, dict) or not isinstance(primary.get("name"), str):
        raise ManifestError("asset must be an object with a name")
    if primary["name"] not in assets:
        raise ManifestError(f"asset.name {primary['name']!r} is not listed in assets")
    expected = {"name": primary["name"], **assets[primary["name"]]}
    if primary != expected:
        raise ManifestError("asset must restate the matching assets entry")


def dumps(manifest: dict) -> str:
    # Stable key order and a trailing newline: the branch writer commits only
    # when the file changed, and a re-run for the same release must not
    # produce a different byte sequence.
    return json.dumps(manifest, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="cmd", required=True)

    b = sub.add_parser("build", help="write the manifest for one release")
    b.add_argument("--component", required=True, help="release-please component, e.g. gawk-broadcast")
    b.add_argument("--version", required=True, help="X.Y.Z as in .release-please-manifest.json")
    b.add_argument("--tag", required=True, help="the release tag, e.g. gawk-broadcast/v1.13.0")
    b.add_argument("--published-at", required=True, help="the release's published_at, RFC 3339 UTC")
    b.add_argument("--asset-dir", required=True, type=Path, help="directory of the files attached to the release")
    b.add_argument("--primary", required=True, help="asset name the download button points at")
    b.add_argument("--repo", default=REPO)
    b.add_argument("--out", required=True, type=Path, help="where to write latest.json")

    v = sub.add_parser("validate", help="check a manifest's shape")
    v.add_argument("path", type=Path)

    args = parser.parse_args(argv)
    try:
        if args.cmd == "build":
            manifest = build(args.component, args.version, args.tag, args.published_at,
                             args.asset_dir, args.primary, args.repo)
            args.out.parent.mkdir(parents=True, exist_ok=True)
            args.out.write_text(dumps(manifest))
            print(f"wrote {args.out}: {manifest['tag']} -> {manifest['asset']['name']}")
        else:
            validate(json.loads(args.path.read_text()))
            print(f"{args.path}: ok")
    except (ManifestError, OSError, json.JSONDecodeError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
