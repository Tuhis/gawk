package version

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestComposeFormats(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rev      string
		modified bool
		want     string
	}{
		{"no vcs info", "", false, "1.9.0"},
		{"no vcs info, modified flag ignored", "", true, "1.9.0"},
		{"abbreviates the revision", "2986a7ffc842204b0648b4cb0e0082642d0bd78f", false, "1.9.0+g2986a7f"},
		{"dirty tree", "2986a7ffc842204b0648b4cb0e0082642d0bd78f", true, "1.9.0+g2986a7f.dirty"},
		{"short revision is used whole", "abc12", false, "1.9.0+gabc12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compose("1.9.0", tc.rev, tc.modified); got != tc.want {
				t.Errorf("compose(%q, %v) = %q, want %q", tc.rev, tc.modified, got, tc.want)
			}
		})
	}
}

// The build string always leads with the release, whatever the VCS situation of
// the machine running the tests — the window header and the diagnostics dump
// both rely on that prefix being the thing you can look up in the changelog.
func TestStringStartsWithRelease(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, Release) {
		t.Fatalf("String() = %q, want it to start with Release %q", got, Release)
	}
	if rest := strings.TrimPrefix(got, Release); rest != "" && !strings.HasPrefix(rest, "+g") {
		t.Errorf("String() = %q: suffix %q is neither empty nor a +g revision", got, rest)
	}
}

// The R2 lesson, applied to versioning: a knob that is wired everywhere except
// where it ships is worse than no knob. release-please updates Release through
// the generic updater (release-please-config.json extra-files) — a mechanism
// that fails *silently* if the annotation comment is reformatted or the config
// entry is dropped, leaving the window showing a version that has not existed
// for six releases. This test turns that into a red release PR.
func TestReleaseMatchesManifest(t *testing.T) {
	const manifestPath = "../../../.release-please-manifest.json"
	raw, err := os.ReadFile(manifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		// Building from a source tarball of this module alone. Nothing to
		// check against, and nothing wrong.
		t.Skipf("no %s — not a full repo checkout", manifestPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	want, ok := manifest["gawk-broadcast"]
	if !ok {
		t.Fatalf("%s has no gawk-broadcast entry", manifestPath)
	}
	if Release != want {
		t.Errorf("Release = %q but %s says %q — release-please updated one and not the other; check the extra-files entry and the x-release-please-version comment in version.go", Release, manifestPath, want)
	}
}
