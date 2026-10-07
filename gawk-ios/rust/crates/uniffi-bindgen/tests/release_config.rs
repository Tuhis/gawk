//! The `gawk-ios` release bumps `rust/Cargo.lock` through a `toml` extra-file
//! whose jsonpath names the workspace's crates one by one: release-please's
//! `simple` type rewrites only the annotated line in Cargo.toml. A crate
//! missing from that list keeps its old version in the lock, and the release
//! PR fails every `--locked` step (gawk-devpub, PR #467). Same guard as the
//! desktop workspace's `every_workspace_crate_is_in_the_lockfile_bump`.
//! Lives here because this crate is pure Rust and builds on any host.

use std::path::Path;

const WORKSPACE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../..");

/// The workspace's own crates, by package name, from its `members` list.
fn workspace_crates() -> Vec<String> {
    let root = Path::new(WORKSPACE);
    let manifest = std::fs::read_to_string(root.join("Cargo.toml")).expect("workspace Cargo.toml");
    let members = manifest
        .split_once("members = [")
        .and_then(|(_, rest)| rest.split_once(']'))
        .map(|(list, _)| list)
        .expect("a members list");
    members
        .split(',')
        .map(|m| m.trim().trim_matches('"'))
        .filter(|m| !m.is_empty())
        .map(|member| {
            let crate_manifest = std::fs::read_to_string(root.join(member).join("Cargo.toml"))
                .unwrap_or_else(|e| panic!("{member}/Cargo.toml: {e}"));
            crate_manifest
                .lines()
                .find_map(|l| l.strip_prefix("name = "))
                .unwrap_or_else(|| panic!("{member} has no package name"))
                .trim_matches('"')
                .to_string()
        })
        .collect()
}

#[test]
fn every_workspace_crate_is_in_the_lockfile_bump() {
    let path = Path::new(WORKSPACE).join("../../release-please-config.json");
    let Ok(raw) = std::fs::read_to_string(path) else {
        return; // a source tarball of this workspace alone
    };
    let config: serde_json::Value = serde_json::from_str(&raw).expect("config is JSON");
    let jsonpath = config["packages"]["gawk-ios"]["extra-files"]
        .as_array()
        .expect("extra-files")
        .iter()
        .find(|f| f["path"] == "rust/Cargo.lock")
        .and_then(|f| f["jsonpath"].as_str())
        .expect("a rust/Cargo.lock extra-file with a jsonpath");
    let crates = workspace_crates();
    assert!(crates.len() >= 5, "found only {crates:?}");
    for name in crates {
        assert!(
            jsonpath.contains(&format!("@.name.value==\"{name}\"")),
            "{name} is missing from the gawk-ios rust/Cargo.lock jsonpath in release-please-config.json"
        );
    }
}
