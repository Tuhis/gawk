package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repo is a scratch git repository shaped like this one: a release-please
// config, components with UI package.json files, and common-ts packages.
type repo struct {
	t    *testing.T
	root string
}

const testReleaseConfig = `{
  "packages": {
    "gawk-admin": { "release-type": "go", "component": "gawk-admin" },
    "gawk-telemetry": { "release-type": "go", "component": "gawk-telemetry" },
    "gawk-app": { "release-type": "node", "component": "gawk-app" }
  }
}
`

func consumerManifest(deps string) string {
	return `{ "name": "ui", "private": true, "dependencies": {` + deps + `} }` + "\n"
}

const oidcDep = `"@gawk/oidc-session": "file:../../common-ts/oidc-session"`

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, root: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.write(".gitignore", "node_modules/\n")
	r.write(releaseConfig, testReleaseConfig)
	r.write("common-ts/oidc-session/package.json", `{ "name": "@gawk/oidc-session" }`+"\n")
	r.write("common-ts/oidc-session/session.ts", "export const a = 1;\n")
	r.write("gawk-admin/ui/package.json", consumerManifest(oidcDep))
	r.write("gawk-app/package.json", consumerManifest(`"react": "^19"`))
	return r
}

func (r *repo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) read(rel string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

func (r *repo) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(rel)))
	return err == nil
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{
		"-C", r.root,
		"-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
	}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit() {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "test")
}

func (r *repo) check() []Stale {
	r.t.Helper()
	stale, err := Check(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	return stale
}

func (r *repo) update() []string {
	r.t.Helper()
	changed, err := Update(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	return changed
}

func stalePaths(stale []Stale) []string {
	var out []string
	for _, s := range stale {
		out = append(out, s.Path)
	}
	return out
}

func TestTheLockRecordsHEADsTreeHashOnceCommitted(t *testing.T) {
	r := newRepo(t)
	r.commit()

	if got := stalePaths(r.check()); !slices.Equal(got, []string{"gawk-admin/common-ts.lock"}) {
		t.Fatalf("check on a tree with no lock = %v, want the admin lock missing", got)
	}
	if got := r.update(); !slices.Equal(got, []string{"gawk-admin/common-ts.lock"}) {
		t.Fatalf("update wrote %v", got)
	}
	r.commit()

	// The documented contract: the line is what `git rev-parse
	// HEAD:common-ts/<pkg>` prints for the committed package.
	want := r.git("rev-parse", "HEAD:common-ts/oidc-session")
	lock := r.read("gawk-admin/common-ts.lock")
	if !strings.Contains(lock, "\noidc-session "+want+"\n") {
		t.Fatalf("lock does not carry HEAD's tree hash %s:\n%s", want, lock)
	}
	if !strings.HasPrefix(lock, "#") || !strings.Contains(lock, updateCommand) {
		t.Fatalf("lock header does not name the regeneration command:\n%s", lock)
	}
	if stale := r.check(); len(stale) != 0 {
		t.Fatalf("check after update+commit = %v, want fresh", stale)
	}
	// No consumer, no lock: gawk-app depends on nothing in common-ts.
	if r.exists("gawk-app/common-ts.lock") {
		t.Fatal("a component that consumes nothing got a lock file")
	}
}

func TestAnEditToThePackageStalesEveryConsumersLock(t *testing.T) {
	r := newRepo(t)
	r.write("gawk-telemetry/ui/package.json", consumerManifest(oidcDep))
	r.update()
	r.commit()

	// The scratch-branch case from docs/55 TO1: the package moves, the locks
	// do not. Uncommitted, because the hash is the working tree's.
	r.write("common-ts/oidc-session/session.ts", "export const a = 2;\n")
	stale := r.check()
	if got := stalePaths(stale); !slices.Equal(got, []string{"gawk-admin/common-ts.lock", "gawk-telemetry/common-ts.lock"}) {
		t.Fatalf("stale = %v, want both consumers", got)
	}
	if !strings.Contains(stale[0].Reason, "oidc-session is locked at") {
		t.Fatalf("reason does not say which package moved: %q", stale[0].Reason)
	}

	// Edit, update, commit — in that order — is fresh, and stays fresh in the
	// commit: a HEAD-based hash would have recorded the pre-edit tree here.
	r.update()
	r.commit()
	if stale := r.check(); len(stale) != 0 {
		t.Fatalf("check after the natural edit/update/commit order = %v", stale)
	}
	want := r.git("rev-parse", "HEAD:common-ts/oidc-session")
	for _, lock := range []string{"gawk-admin/common-ts.lock", "gawk-telemetry/common-ts.lock"} {
		if !strings.Contains(r.read(lock), want) {
			t.Fatalf("%s does not carry the committed tree %s", lock, want)
		}
	}
}

func TestAConsumerOnlyChangeTouchesNoLock(t *testing.T) {
	r := newRepo(t)
	r.update()
	r.commit()
	r.write("gawk-admin/ui/src/main.tsx", "console.log(1);\n")
	r.write("gawk-admin/ui/package.json", consumerManifest(oidcDep+`, "react": "^19"`))
	if stale := r.check(); len(stale) != 0 {
		t.Fatalf("a consumer-only change staled %v", stale)
	}
	if changed := r.update(); len(changed) != 0 {
		t.Fatalf("a consumer-only change rewrote %v", changed)
	}
}

func TestIgnoredFilesDoNotMoveTheHash(t *testing.T) {
	r := newRepo(t)
	r.update()
	r.commit()
	// What git would never commit cannot be part of the package's identity:
	// a stray node_modules inside it, say, from someone running npm there.
	r.write("common-ts/oidc-session/node_modules/x/index.js", "1\n")
	if stale := r.check(); len(stale) != 0 {
		t.Fatalf("an ignored file staled %v", stale)
	}
	// An untracked file that is NOT ignored would be committed, so it does.
	r.write("common-ts/oidc-session/extra.ts", "export {};\n")
	if stale := r.check(); len(stale) != 1 {
		t.Fatalf("an untracked, unignored file staled %v, want the admin lock", stale)
	}
}

func TestTheCheckLeavesTheRealIndexAlone(t *testing.T) {
	r := newRepo(t)
	r.commit()
	r.write("common-ts/oidc-session/session.ts", "export const a = 3;\n")
	before := r.git("ls-files", "--stage")
	r.check()
	r.update()
	if after := r.git("ls-files", "--stage"); after != before {
		t.Fatalf("the tool changed the real index:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if staged := r.git("diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("the tool staged %s", staged)
	}
}

func TestOneLockPerComponentWhateverTheNumberOfManifests(t *testing.T) {
	r := newRepo(t)
	r.write("common-ts/other/package.json", `{ "name": "@gawk/other" }`+"\n")
	r.write("gawk-admin/tools/package.json", consumerManifest(`"@gawk/other": "file:../../common-ts/other/"`))
	r.update()
	lock := r.read("gawk-admin/common-ts.lock")
	var pkgs []string
	for _, line := range strings.Split(lock, "\n") {
		if f := strings.Fields(line); len(f) == 2 && !strings.HasPrefix(line, "#") {
			pkgs = append(pkgs, f[0])
		}
	}
	if !slices.Equal(pkgs, []string{"oidc-session", "other"}) {
		t.Fatalf("packages in the admin lock = %v, want both, sorted:\n%s", pkgs, lock)
	}
}

func TestAnOrphanedLockIsStaleAndUpdateRemovesIt(t *testing.T) {
	r := newRepo(t)
	r.update()
	r.commit()
	// The admin UI stops consuming the package.
	r.write("gawk-admin/ui/package.json", consumerManifest(`"react": "^19"`))
	if got := stalePaths(r.check()); !slices.Equal(got, []string{"gawk-admin/common-ts.lock"}) {
		t.Fatalf("stale = %v, want the orphaned lock", got)
	}
	r.update()
	if r.exists("gawk-admin/common-ts.lock") {
		t.Fatal("update left the orphaned lock behind")
	}
}

func TestDiscoverySkipsNodeModulesAndCommonTs(t *testing.T) {
	r := newRepo(t)
	// Ignored by .gitignore, so never listed — even though it names the
	// package exactly as a consumer would.
	r.write("gawk-telemetry/ui/node_modules/x/package.json", consumerManifest(oidcDep))
	// A common-ts package is a dependency, never a consumer.
	r.write("common-ts/third/package.json", consumerManifest(`"@gawk/oidc-session": "file:../oidc-session"`))
	cs, err := Consumers(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Manifest != "gawk-admin/ui/package.json" || cs[0].Component != "gawk-admin" {
		t.Fatalf("consumers = %+v, want only gawk-admin/ui", cs)
	}
}

func TestMisplacedConsumersAndSpecsAreErrors(t *testing.T) {
	cases := map[string]struct {
		manifest, deps, wantErr string
	}{
		"outside every component": {
			"site/package.json", `"@gawk/oidc-session": "file:../common-ts/oidc-session"`,
			"no release-please component",
		},
		"deeper than a package": {
			"gawk-admin/ui/package.json", `"@gawk/oidc-session": "file:../../common-ts/oidc-session/src"`,
			"not to a package directly under common-ts/",
		},
		"a package that does not exist": {
			"gawk-admin/ui/package.json", `"@gawk/nope": "file:../../common-ts/nope"`,
			"not a directory",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			r.write(tc.manifest, consumerManifest(tc.deps))
			_, err := Check(r.root)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Check error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAFileDependencyElsewhereIsNotAConsumer(t *testing.T) {
	r := newRepo(t)
	r.write("gawk-app/package.json", consumerManifest(`"local": "file:./vendor/local"`))
	cs, err := Consumers(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("consumers = %+v, want only gawk-admin/ui", cs)
	}
}

func TestTheDeepestComponentOwnsTheConsumer(t *testing.T) {
	got := owningComponent([]string{".", "gawk-admin", "gawk-admin/ui", "gawk-app"}, "gawk-admin/ui")
	if got != "gawk-admin/ui" {
		t.Fatalf("owner = %q", got)
	}
	if got := owningComponent([]string{".", "gawk-admin"}, "gawk-administrator/ui"); got != "" {
		t.Fatalf("a prefix that is not a directory boundary matched: %q", got)
	}
}
