"""Tests for sign-sums.sh (R47, docs/48 SU1).

The attach job that signs never runs on a pull request, so this is where
the script meets a runner before a release depends on it: the pinned
minisign download, the hash pin, sign and verify, with a throwaway key.
"""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / "sign-sums.sh"


def run(*args, env=None):
    full = {k: v for k, v in os.environ.items() if not k.startswith("MINISIGN")}
    full.update(env or {})
    return subprocess.run(
        ["bash", str(SCRIPT), *args], env=full, capture_output=True, text=True
    )


class SignSums(unittest.TestCase):
    def test_self_test_signs_and_verifies_with_the_pinned_minisign(self):
        r = run("--self-test")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("Trusted comment: gawk-broadcast-desktop 0.0.1", r.stdout)

    def test_a_missing_secret_fails_before_anything_is_signed(self):
        with tempfile.TemporaryDirectory() as d:
            Path(d, "SHA256SUMS").write_text("x  ./a\n")
            r = run(d, "2.1.0", env={"MINISIGN_PASSWORD": "p"})
            self.assertEqual(r.returncode, 1)
            self.assertIn("never attached unsigned", r.stderr)
            self.assertFalse(Path(d, "SHA256SUMS.minisig").exists())

    def test_only_a_release_version_is_signed(self):
        with tempfile.TemporaryDirectory() as d:
            Path(d, "SHA256SUMS").write_text("x  ./a\n")
            for bad in ("2.1", "v2.1.0", "2.1.0-rc.1", "2.1.0 extra"):
                r = run(d, bad, env={"MINISIGN_SECRET_KEY": "k", "MINISIGN_PASSWORD": "p"})
                self.assertEqual(r.returncode, 1, bad)
                self.assertIn("not a release version", r.stderr)

    def test_the_checked_in_key_is_the_one_the_apps_compile_in(self):
        pub = (HERE / "keys" / "gawk-release.pub").read_text().splitlines()
        self.assertEqual(pub[0], "untrusted comment: minisign public key EFB62FBC86D13877")
        update_rs = (
            HERE.parents[1] / "gawk-broadcast-desktop" / "crates" / "engine" / "src" / "update.rs"
        ).read_text()
        self.assertIn(f'pub const RELEASE_KEY: &str = "{pub[1]}";', update_rs)


if __name__ == "__main__":
    unittest.main()
