import hashlib
import json
import tempfile
import unittest
from pathlib import Path

import manifest as m


def _fixture(root: Path, names=("gawk-broadcast-linux-amd64.tar.gz", "SHA256SUMS")) -> Path:
    d = root / "assets"
    d.mkdir()
    for i, n in enumerate(names):
        (d / n).write_bytes(b"x" * (i + 1) * 100)
    return d


class Build(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.assets = _fixture(self.root)
        self.addCleanup(self.tmp.cleanup)

    def build(self, **kw):
        args = dict(component="gawk-broadcast", version="1.13.0", tag="gawk-broadcast/v1.13.0",
                    published_at="2026-09-10T19:54:22Z", asset_dir=self.assets,
                    primary="gawk-broadcast-linux-amd64.tar.gz")
        args.update(kw)
        return m.build(**args)

    def test_lists_every_asset_with_real_checksums(self):
        out = self.build()
        self.assertEqual(sorted(out["assets"]), ["SHA256SUMS", "gawk-broadcast-linux-amd64.tar.gz"])
        tarball = out["assets"]["gawk-broadcast-linux-amd64.tar.gz"]
        self.assertEqual(tarball["size"], 100)
        self.assertEqual(tarball["sha256"], hashlib.sha256(b"x" * 100).hexdigest())
        self.assertEqual(
            tarball["url"],
            "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast/v1.13.0/gawk-broadcast-linux-amd64.tar.gz")

    def test_primary_restates_its_assets_entry(self):
        out = self.build()
        self.assertEqual(out["asset"], {"name": "gawk-broadcast-linux-amd64.tar.gz",
                                        **out["assets"]["gawk-broadcast-linux-amd64.tar.gz"]})
        self.assertEqual(out["release_url"], "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast/v1.13.0")
        self.assertEqual(out["schema"], m.SCHEMA)

    def test_primary_must_exist(self):
        with self.assertRaisesRegex(m.ManifestError, "primary asset"):
            self.build(primary="nope.exe")

    def test_tag_must_match_component_and_version(self):
        with self.assertRaisesRegex(m.ManifestError, "does not match"):
            self.build(tag="gawk-broadcast/v1.12.0")
        with self.assertRaisesRegex(m.ManifestError, "does not match"):
            self.build(tag="gawk-broadcast-windows/v1.13.0")

    def test_legacy_tag_spelling_is_accepted(self):
        # Releases cut before the tag-separator change are `component-vX.Y.Z`,
        # and a backfill of one runs the writer on exactly that tag.
        out = self.build(tag="gawk-broadcast-v1.13.0")
        self.assertTrue(out["asset"]["url"].startswith(
            "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-v1.13.0/"))

    def test_empty_dir_is_an_error(self):
        empty = self.root / "empty"
        empty.mkdir()
        with self.assertRaisesRegex(m.ManifestError, "no files"):
            self.build(asset_dir=empty)

    def test_dumps_is_stable(self):
        a, b = self.build(), self.build()
        self.assertEqual(m.dumps(a), m.dumps(b))
        self.assertTrue(m.dumps(a).endswith("}\n"))
        self.assertEqual(json.loads(m.dumps(a)), a)


class Validate(unittest.TestCase):
    def good(self):
        a = {"size": 10, "sha256": "0" * 64, "url": "https://x/releases/download/c/v1.0.0/f"}
        return {"schema": 1, "component": "c", "version": "1.0.0", "tag": "c/v1.0.0",
                "published_at": "2026-01-02T03:04:05Z", "release_url": "https://x/releases/tag/c/v1.0.0",
                "asset": {"name": "f", **a}, "assets": {"f": a}}

    def test_good_passes(self):
        m.validate(self.good())

    def test_rejects(self):
        cases = {
            "schema": lambda d: d.update(schema=2),
            "version": lambda d: d.update(version="1.0", tag="c/v1.0"),
            "timestamp": lambda d: d.update(published_at="2026-01-02 03:04:05"),
            "sha256": lambda d: d["assets"]["f"].update(sha256="abc"),
            "size": lambda d: d["assets"]["f"].update(size=0),
            "url": lambda d: d["assets"]["f"].update(url="https://x/other"),
            "primary-missing": lambda d: d["asset"].update(name="g"),
            "primary-stale": lambda d: d["asset"].update(size=11),
            "no-assets": lambda d: d.update(assets={}),
            "not-an-object": lambda d: d.clear(),
        }
        for name, mutate in cases.items():
            with self.subTest(name):
                d = self.good()
                mutate(d)
                with self.assertRaises(m.ManifestError):
                    m.validate(d)

    def test_extra_keys_are_fine(self):
        d = self.good()
        d["notes"] = "a later schema may add keys"
        m.validate(d)


class Cli(unittest.TestCase):
    def test_build_then_validate(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            assets = _fixture(root, ("gawk-broadcast-windows-x86_64.exe", "INSTALL.md", "SHA256SUMS"))
            out = root / "releases" / "gawk-broadcast-windows" / "latest.json"
            rc = m.main(["build", "--component", "gawk-broadcast-windows", "--version", "1.3.0",
                         "--tag", "gawk-broadcast-windows/v1.3.0", "--published-at", "2026-09-10T19:54:23Z",
                         "--asset-dir", str(assets), "--primary", "gawk-broadcast-windows-x86_64.exe",
                         "--out", str(out)])
            self.assertEqual(rc, 0)
            self.assertEqual(m.main(["validate", str(out)]), 0)
            data = json.loads(out.read_text())
            self.assertEqual(data["asset"]["name"], "gawk-broadcast-windows-x86_64.exe")
            self.assertEqual(len(data["assets"]), 3)

    def test_validate_reports_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "bad.json"
            p.write_text("{}")
            self.assertEqual(m.main(["validate", str(p)]), 1)


if __name__ == "__main__":
    unittest.main()
