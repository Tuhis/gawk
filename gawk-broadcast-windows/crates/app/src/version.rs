//! Which build of the broadcaster is running.
//!
//! Two values, deliberately not one — the same split as the Go broadcaster's
//! `internal/version`:
//!
//! * [`RELEASE`] is the workspace version release-please maintains in
//!   `Cargo.toml`. It is what goes on the telemetry wire, because
//!   gawk-telemetry groups sessions by `appVersion` and a per-build suffix
//!   would shatter every group into rows of one (docs/33 D15).
//! * [`display`] is what a human reads: the release plus the commit it was
//!   built from. There is no tag-triggered release build for this component —
//!   every EXE anyone runs is a CI artifact off main or a PR, or a local
//!   build — so a bare "1.0.0" in the window would be a lie on every copy in
//!   existence. The commit is the part that identifies the build.
//!
//! No `.dirty` marker here, unlike the Linux side. Go's `-buildvcs` recomputes
//! cleanliness on every build; a `git status` in build.rs would be frozen at
//! the last time Cargo decided to rerun the script, and a *stale* dirty flag
//! is worse than none — it is the one thing you would trust when checking
//! whether your edit made it into the binary.

/// The last released version of this component, maintained by release-please
/// in the workspace `Cargo.toml`.
pub const RELEASE: &str = env!("CARGO_PKG_VERSION");

/// git's classic abbreviation: unique enough in a repo this size, short enough
/// to sit in a window header.
const SHORT_REV_LEN: usize = 7;

/// The build string: `1.0.0+g1a2b3c4`, or a bare `1.0.0` when the build had no
/// VCS information (a source-tarball build).
///
/// No leading `v` — callers that want one prepend it. This value also lands in
/// the diagnostics JSON, where a `v` would be noise.
pub fn display() -> String {
    compose(RELEASE, option_env!("GAWK_BUILD_REV").unwrap_or(""))
}

/// The whole format, split out from the environment lookup so it can be tested
/// without rebuilding the crate under different env vars.
fn compose(release: &str, rev: &str) -> String {
    let rev = rev.trim();
    if rev.is_empty() {
        // Nothing to add, and "+gunknown" would be worse than silence: it
        // looks like a commit until you try to look it up.
        return release.to_string();
    }
    let short: String = rev.chars().take(SHORT_REV_LEN).collect();
    // "+" is SemVer build metadata, ignored for precedence — which is the
    // honest reading: this is the 1.0.0 line, at this commit. A "-dev."
    // pre-release suffix would sort *before* 1.0.0, i.e. backwards.
    format!("{release}+g{short}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compose_formats() {
        assert_eq!(compose("1.0.0", ""), "1.0.0");
        assert_eq!(compose("1.0.0", "   "), "1.0.0");
        assert_eq!(
            compose("1.0.0", "2986a7ffc842204b0648b4cb0e0082642d0bd78f"),
            "1.0.0+g2986a7f"
        );
        assert_eq!(compose("1.0.0", "abc12"), "1.0.0+gabc12");
    }

    /// Whatever the VCS situation of the machine running the tests, the build
    /// string leads with the release — the window header and the diagnostics
    /// dump both rely on that prefix being the thing you can look up in the
    /// changelog.
    #[test]
    fn display_starts_with_the_release() {
        let got = display();
        assert!(
            got.starts_with(RELEASE),
            "{got} should start with {RELEASE}"
        );
        let rest = &got[RELEASE.len()..];
        assert!(
            rest.is_empty() || rest.starts_with("+g"),
            "{got}: suffix {rest} is neither empty nor a +g revision"
        );
    }

    /// The R2 lesson, applied to versioning: a value that is wired everywhere
    /// except where it ships is worse than no value. `RELEASE` comes from the
    /// workspace `Cargo.toml`, which release-please updates through a generic
    /// extra-files entry — a mechanism that fails *silently* if the annotation
    /// comment is reformatted or the config entry is dropped, leaving the
    /// window showing a version that has not existed for six releases. This
    /// turns that into a red release PR.
    #[test]
    fn release_matches_the_manifest() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../.release-please-manifest.json"
        );
        let Ok(raw) = std::fs::read_to_string(path) else {
            // Building from a source tarball of this workspace alone. Nothing
            // to check against, and nothing wrong.
            return;
        };
        let manifest: serde_json::Value = serde_json::from_str(&raw).expect("manifest is JSON");
        let want = manifest["gawk-broadcast-windows"]
            .as_str()
            .expect("manifest has a gawk-broadcast-windows entry");
        assert_eq!(
            RELEASE, want,
            "Cargo.toml says {RELEASE} but .release-please-manifest.json says {want} — \
             release-please updated one and not the other; check the extra-files entry \
             and the x-release-please-version comment in the workspace Cargo.toml"
        );
    }
}
