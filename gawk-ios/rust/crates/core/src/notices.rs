//! The app's third-party notices for Settings → Open-source licences (R72,
//! docs/74 D2): `gawk-ios/THIRD-PARTY-NOTICES.md`, embedded in the core and
//! read by the desktop's `gawk_engine::notices`, so the file the generator
//! writes is the file the app shows and one parser serves every app.

use gawk_engine::notices;

const FILE: &str = include_str!("../../../../THIRD-PARTY-NOTICES.md");

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct NoticePackage {
    pub name: String,
    /// Empty when the file gives none.
    pub version: String,
    /// As declared: an SPDX expression.
    pub license: String,
    pub copyright: Vec<String>,
}

/// Where an MPL package's source can be had (MPL-2.0 §3.2(a)).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct NoticeSource {
    pub package: String,
    pub url: String,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct NoticeText {
    pub license: String,
    pub applies_to: String,
    pub body: String,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct Notices {
    /// The generator's paragraphs on what ships, blank-line separated.
    pub intro: String,
    pub packages: Vec<NoticePackage>,
    pub sources: Vec<NoticeSource>,
    pub texts: Vec<NoticeText>,
}

/// The notices, parsed. Swift asks once, when the page first opens.
#[uniffi::export]
pub fn third_party_notices() -> Notices {
    let n = notices::parse(FILE);
    Notices {
        intro: n.intro,
        packages: n
            .packages
            .into_iter()
            .map(|p| NoticePackage {
                name: p.name,
                version: p.version,
                license: p.license,
                copyright: p.copyright,
            })
            .collect(),
        sources: n
            .sources
            .into_iter()
            .map(|s| NoticeSource {
                package: s.package,
                url: s.url,
            })
            .collect(),
        texts: n
            .texts
            .into_iter()
            .map(|t| NoticeText {
                license: t.license,
                applies_to: t.applies_to,
                body: t.body,
            })
            .collect(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_app_shows_its_own_file_whole() {
        let n = third_party_notices();
        assert!(
            n.intro.contains("iOS app"),
            "the iOS file, not a desktop one"
        );
        assert!(n.packages.len() > 100);
        for native in ["libopus", "libvpx"] {
            assert!(n.packages.iter().any(|p| p.name == native), "{native}");
        }
        assert!(
            n.packages
                .iter()
                .any(|p| p.name == "uniffi" && p.license == "MPL-2.0")
        );
        assert!(n.sources.iter().any(|s| s.package.starts_with("uniffi ")));
        assert!(
            n.texts
                .iter()
                .any(|t| t.body.contains("Mozilla Public License Version 2.0"))
        );
    }

    // R74 (docs/76 item 28): the shared engine's user-facing copy for 4012
    // PopUplinkFull, pinned on the iOS side because this core reaches the
    // engine and wire crates by path (CONTRIBUTING.md). 4012 is not terminal
    // for a viewer: the iOS viewer keeps retrying.
    #[test]
    fn pop_uplink_full_copy_is_pinned_and_not_terminal() {
        assert_eq!(
            gawk_engine::resume::close_code_message(gawk_wire::CLOSE_CODE_POP_UPLINK_FULL),
            "This stream is at a LAN party with limited internet. Outside viewing is full right now; we'll keep trying."
        );
        assert_eq!(gawk_wire::CLOSE_CODE_POP_UPLINK_FULL, 4012);
        assert!(!gawk_wire::terminal_for_viewer(
            gawk_wire::CLOSE_CODE_POP_UPLINK_FULL
        ));
    }
}
