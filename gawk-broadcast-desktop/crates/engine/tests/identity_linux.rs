//! The injected identity (R56, docs/58 D2), in its own test binary: `set_this`
//! writes a process-wide global, and a test binary is a process, so nothing
//! here can leak into the unit tests' "unset" assertion.

use gawk_engine::config::Config;
use gawk_engine::defaults::{self, LINUX, WINDOWS};

#[test]
fn the_linux_shell_injects_its_identity_and_a_second_one_panics() {
    // Before injection: today's rule (Windows on a Linux host).
    if !cfg!(target_os = "macos") {
        assert_eq!(defaults::this(), &WINDOWS);
    }

    defaults::set_this(&LINUX);
    assert_eq!(defaults::this(), &LINUX);
    assert_eq!(defaults::origin(), "gawk-broadcast://linux");
    assert_eq!(Config::default().resolve_origin(), "gawk-broadcast://linux");

    // Idempotent for the same value.
    defaults::set_this(&LINUX);
    assert_eq!(defaults::this(), &LINUX);

    // A different value is two shells racing: refused loudly.
    let second = std::panic::catch_unwind(|| defaults::set_this(&WINDOWS));
    assert!(second.is_err(), "a conflicting set_this must panic");
    assert_eq!(defaults::this(), &LINUX, "and the first identity stands");
}
