// Package version reports which build of the broadcaster is running.
//
// Two values, deliberately not one:
//
//   - [Release] is the component version release-please maintains. It is what
//     goes on the telemetry wire, because gawk-telemetry groups sessions by
//     appVersion (readapi/trends.go) and a per-build suffix would shatter every
//     group into rows of one. gawk-app treats its package.json version the same
//     way (docs/33 D15).
//   - [String] is what a human reads: the release plus the commit it was built
//     from. Neither broadcaster has a tag-triggered release build — every binary
//     anyone runs is a CI artifact off main or a PR, or a local build — so a bare
//     "1.9.0" in the window would be a lie on every copy in existence. The commit
//     is the part that identifies the build.
//
// The commit comes from the Go toolchain's own VCS stamping (-buildvcs, on by
// default), not from ldflags or `git describe`: it costs no build plumbing, it
// is accurate about a dirty tree, and it needs no tags — which matters because
// CI checks out at fetch-depth 1 and has none.
//
// One local-dev gotcha, measured rather than assumed: the go tool walks up for
// the nearest `.git`, and a git worktree created *inside* the main checkout
// (as .claude/worktrees/* are) is overtaken by the outer repo's real `.git`
// directory. Builds from such a worktree therefore stamp the MAIN checkout's
// commit and cleanliness, so the badge can read `.dirty`-free while your
// worktree is filthy, or name a commit you are not on. It is right in CI and
// right for a normal clone; if a local build's suffix looks wrong, that
// nesting is why.
package version

import (
	"runtime/debug"
	"sync"
)

// Release is the last released version of this component.
//
// Maintained by release-please via the extra-files entry in
// release-please-config.json; the annotation comment is the hook it looks for,
// so don't reformat this line. TestReleaseMatchesManifest is the proof that the
// hook still bites.
const Release = "1.11.0" // x-release-please-version

// shortRevLen is git's classic abbreviation. Long enough to be unique in a
// repo this size, short enough to sit in a window header.
const shortRevLen = 7

// String is the build string: "1.9.0+g1a2b3c4", or "1.9.0+g1a2b3c4.dirty" when
// the tree had uncommitted changes, or a bare "1.9.0" when there is no VCS
// information at all (a source-tarball build, or -buildvcs=false).
//
// No leading "v" — callers that want one prepend it. This value also lands in
// the diagnostics JSON, where a "v" would be noise.
var String = sync.OnceValue(func() string {
	rev, modified := buildVCS()
	return compose(Release, rev, modified)
})

// compose is the whole format, kept separate from the build-info lookup so it
// can be tested without building binaries.
func compose(release, rev string, modified bool) string {
	if rev == "" {
		// Nothing to add, and "+gunknown" would be worse than silence: it
		// looks like a commit until you try to look it up.
		return release
	}
	if len(rev) > shortRevLen {
		rev = rev[:shortRevLen]
	}
	// "+" is SemVer build metadata, ignored for precedence — which is the
	// honest reading: this is the 1.9.0 line, at this commit. A "-dev."
	// pre-release suffix would sort *before* 1.9.0, i.e. backwards.
	s := release + "+g" + rev
	if modified {
		s += ".dirty"
	}
	return s
}

// buildVCS reads the revision the Go toolchain stamped in. Absent for `go run`,
// for test binaries, and for builds outside a checkout — all of which fall back
// to the bare release version.
func buildVCS() (rev string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return rev, modified
}
