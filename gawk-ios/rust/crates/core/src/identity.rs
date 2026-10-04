//! The iOS app's distribution identity (docs/67 D5), injected into the
//! shared engine with `set_this` the way the Linux shell injects its own
//! (docs/58 D2). It lives here, not in the desktop crates (D4: no iOS code
//! in them).

use gawk_engine::defaults::Distribution;

/// Every string here is matched verbatim outside this workspace: `name` by
/// `gawk-telemetry`'s accepted kinds (IO7), `origin` by the production
/// relay's `-allowed-origins`, `app` and `os` by the relay's R59 metrics
/// vocabulary (docs/61 D1). R65 publishes no artifact (OD3), so there is no
/// release asset.
pub const IOS: Distribution = Distribution {
    name: "gawk-ios",
    origin: "gawk://ios",
    asset: "",
    os: "iOS",
    app: "ios",
};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_ios_identity_is_pinned() {
        assert_eq!(
            IOS,
            Distribution {
                name: "gawk-ios",
                origin: "gawk://ios",
                asset: "",
                os: "iOS",
                app: "ios",
            }
        );
    }
}
