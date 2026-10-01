//! Stamps the commit as `GAWK_BUILD_REV`, for the version badge in the window
//! header (see src/version.rs). NOT gawk-ui's build script: each shell's
//! build.rs includes this file with `#[path]` and its `main` hands the value
//! to `gawk_ui::version::set_build_rev`.
//!
//! Why the shells and not gawk-ui, which is where the badge is drawn: a
//! `cargo::rustc-env` value is part of the crate's compile, so stamping
//! gawk-ui made the workspace's most expensive crate (the generated window,
//! ~100 s of a 16-core build and ~4.5 min of the CI `linux` job) a cache miss
//! on every commit, in every CI job, whether or not anything it compiles had
//! changed. The shells are leaves that recompile on every build anyway.
//!
//! Shared by path rather than as a crate: a new workspace crate would have to
//! join release-please's Cargo.lock bump for no gain.

use std::path::Path;
use std::process::Command;

/// The Go broadcaster gets this free from `-buildvcs`; Cargo has no
/// equivalent, so this is the whole mechanism. Deliberately NOT
/// `git describe`: that needs tags, and CI checks out at fetch-depth 1.
///
/// Nothing here may fail the build. A source-tarball build has no `.git`, and
/// a version badge is not worth a broken build — the crate falls back to the
/// bare release version when the variable is unset.
pub fn emit() {
    // CI first: the checkout is detached and shallow, but GITHUB_SHA is exact
    // and free.
    println!("cargo::rerun-if-env-changed=GITHUB_SHA");
    if let Ok(sha) = std::env::var("GITHUB_SHA")
        && !sha.trim().is_empty()
    {
        println!("cargo::rustc-env=GAWK_BUILD_REV={}", sha.trim());
        return;
    }

    let Some(rev) = git(&["rev-parse", "HEAD"]) else {
        // No git, or not a checkout. Silence beats "+gunknown".
        return;
    };
    println!("cargo::rustc-env=GAWK_BUILD_REV={rev}");

    // Rerun triggers. HEAD alone is not enough: committing on a branch
    // rewrites refs/heads/<branch> and leaves HEAD's mtime untouched, so
    // without the ref file the badge would keep naming the commit you were on
    // when you last edited the shell. `--git-path` is what resolves both
    // through a worktree's gitdir and out to the common dir where refs live.
    //
    // Emitted only when the path exists: Cargo treats a missing
    // rerun-if-changed path as "changed", which would rerun the script on
    // every single build.
    for p in rerun_paths() {
        if Path::new(&p).exists() {
            println!("cargo::rerun-if-changed={p}");
        }
    }
}

fn rerun_paths() -> Vec<String> {
    let mut paths = Vec::new();
    if let Some(head) = git(&["rev-parse", "--git-path", "HEAD"]) {
        paths.push(head);
    }
    // On a branch: the loose ref, or packed-refs if it has been packed.
    //
    // A detached HEAD — which is what a CI checkout leaves you on — has no
    // symbolic ref and needs none: HEAD holds the sha itself and moves with it,
    // so the HEAD path above already covers that case.
    if let Some(refname) = git(&["symbolic-ref", "--quiet", "HEAD"])
        && let Some(p) = git(&["rev-parse", "--git-path", &refname])
    {
        if Path::new(&p).exists() {
            paths.push(p);
        } else if let Some(packed) = git(&["rev-parse", "--git-path", "packed-refs"]) {
            paths.push(packed);
        }
    }
    paths
}

fn git(args: &[&str]) -> Option<String> {
    let out = Command::new("git").args(args).output().ok()?;
    if !out.status.success() {
        return None;
    }
    let s = String::from_utf8(out.stdout).ok()?.trim().to_string();
    if s.is_empty() { None } else { Some(s) }
}
