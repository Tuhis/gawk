//! `initialize_core` injects the iOS identity (docs/67 D5). In its own test binary:
//! `set_this` writes a process-wide global, and a test binary is a process.

use gawk_engine::defaults;
use gawk_engine::relay::{CLIENT_OS, publish_url};

#[test]
fn initialize_core_makes_every_dial_say_gawk_ios() {
    gawk_core::initialize_core();
    assert_eq!(defaults::this(), &gawk_core::identity::IOS);
    assert_eq!(defaults::origin(), "gawk://ios");

    // Idempotent: a second call is harmless.
    gawk_core::initialize_core();

    // The R59 labels on a dial (docs/61 D1): `app=ios`, and the build
    // target's `os`, which is `ios` on a device or simulator build.
    let url = publish_url(defaults::RELAY_URL, "", "", "").unwrap();
    assert!(
        url.ends_with(&format!("/publish?app=ios&os={CLIENT_OS}")),
        "{url}"
    );

    let info = gawk_core::core_info();
    assert_eq!(info.distribution, "gawk-ios");
    assert_eq!(info.origin, "gawk://ios");
    assert_eq!(info.default_relay_url, "https://api.gawk.ioio.fi:4433");
    assert_eq!(info.default_app_url, "https://gawk.ioio.fi");
    assert_eq!(info.version, env!("CARGO_PKG_VERSION"));
}
