package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	// commonDir is the shared TypeScript root (docs/55 OD5a).
	commonDir = "common-ts"
	// lockName is the lock file's name inside each consumer component.
	lockName = "common-ts.lock"
	// releaseConfig maps component directories to release units.
	releaseConfig = "release-please-config.json"
)

// header opens every lock file. Not parsed: the check compares whole files.
const header = "# The common-ts packages this component builds against, by git tree hash\n" +
	"# (docs/55 D10). Generated — do not edit; regenerate with\n" +
	"#   " + updateCommand + "\n"

// Consumer is one package.json that depends on common-ts packages.
type Consumer struct {
	// Manifest is the package.json path, slash-separated and repo-relative.
	Manifest string
	// Component is the release-please package directory that contains it.
	Component string
	// Packages are the common-ts package names it depends on.
	Packages []string
}

// Stale is one lock file whose content disagrees with the tree.
type Stale struct {
	// Path is the lock file, repo-relative.
	Path string
	// Reason says what disagrees, for a human.
	Reason string
}

// Check reports every lock file that `Update` would change, add or delete.
func Check(root string) ([]Stale, error) {
	want, err := expected(root)
	if err != nil {
		return nil, err
	}
	have, err := existingLocks(root)
	if err != nil {
		return nil, err
	}
	var stale []Stale
	for _, p := range sortedKeys(want) {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			stale = append(stale, Stale{p, "missing"})
		case err != nil:
			return nil, err
		case !bytes.Equal(got, []byte(want[p])):
			stale = append(stale, Stale{p, describe(string(got), want[p])})
		}
	}
	for _, p := range have {
		if _, ok := want[p]; !ok {
			stale = append(stale, Stale{p, "no consumer in its component depends on a common-ts package"})
		}
	}
	return stale, nil
}

// Update rewrites every stale lock file and deletes orphaned ones. It returns
// the repo-relative paths it wrote or removed.
func Update(root string) ([]string, error) {
	want, err := expected(root)
	if err != nil {
		return nil, err
	}
	have, err := existingLocks(root)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range sortedKeys(want) {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if got, err := os.ReadFile(abs); err == nil && bytes.Equal(got, []byte(want[p])) {
			continue
		}
		if err := os.WriteFile(abs, []byte(want[p]), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, p)
	}
	for _, p := range have {
		if _, ok := want[p]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			return changed, err
		}
		changed = append(changed, p)
	}
	return changed, nil
}

// expected is every lock file's correct content, keyed by repo-relative path.
func expected(root string) (map[string]string, error) {
	consumers, err := Consumers(root)
	if err != nil {
		return nil, err
	}
	perComponent := map[string]map[string]bool{}
	for _, c := range consumers {
		if perComponent[c.Component] == nil {
			perComponent[c.Component] = map[string]bool{}
		}
		for _, p := range c.Packages {
			perComponent[c.Component][p] = true
		}
	}
	hashes := map[string]string{}
	want := map[string]string{}
	for component, pkgs := range perComponent {
		var b strings.Builder
		b.WriteString(header)
		for _, pkg := range sortedKeys(pkgs) {
			h, ok := hashes[pkg]
			if !ok {
				if h, err = TreeHash(root, pkg); err != nil {
					return nil, err
				}
				hashes[pkg] = h
			}
			fmt.Fprintf(&b, "%s %s\n", pkg, h)
		}
		want[path.Join(component, lockName)] = b.String()
	}
	return want, nil
}

// Consumers discovers every package.json that depends on a common-ts package.
func Consumers(root string) ([]Consumer, error) {
	components, err := releaseComponents(root)
	if err != nil {
		return nil, err
	}
	manifests, err := listFiles(root, "package.json")
	if err != nil {
		return nil, err
	}
	var out []Consumer
	for _, m := range manifests {
		if slices.Contains(strings.Split(m, "/"), "node_modules") ||
			strings.HasPrefix(m, commonDir+"/") {
			continue
		}
		pkgs, err := commonDeps(root, m)
		if err != nil {
			return nil, err
		}
		if len(pkgs) == 0 {
			continue
		}
		component := owningComponent(components, path.Dir(m))
		if component == "" {
			return nil, fmt.Errorf("%s depends on %s but lies in no release-please component (%s), so a lock file for it would release nothing",
				m, strings.Join(pkgs, ", "), releaseConfig)
		}
		out = append(out, Consumer{Manifest: m, Component: component, Packages: pkgs})
	}
	return out, nil
}

// commonDeps returns the common-ts package names a package.json depends on
// through `file:` specs, sorted and de-duplicated.
func commonDeps(root, manifest string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifest)))
	if err != nil {
		return nil, err
	}
	var pj map[string]json.RawMessage
	if err := json.Unmarshal(data, &pj); err != nil {
		return nil, fmt.Errorf("%s: %w", manifest, err)
	}
	found := map[string]bool{}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		raw, ok := pj[field]
		if !ok {
			continue
		}
		var deps map[string]string
		if err := json.Unmarshal(raw, &deps); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", manifest, field, err)
		}
		for name, spec := range deps {
			target, ok := strings.CutPrefix(spec, "file:")
			if !ok {
				continue
			}
			// Resolved against the manifest's directory, as npm does, and then
			// required to land on exactly common-ts/<pkg>: a deeper path or a
			// file is not a package this tool can hash as a unit.
			resolved := path.Clean(path.Join(path.Dir(manifest), filepath.ToSlash(target)))
			rest, ok := strings.CutPrefix(resolved, commonDir+"/")
			if !ok {
				continue // a file: dependency, but not on common-ts
			}
			if rest == "" || strings.Contains(rest, "/") {
				return nil, fmt.Errorf("%s: %s %q resolves to %s, not to a package directly under %s/",
					manifest, name, spec, resolved, commonDir)
			}
			info, err := os.Stat(filepath.Join(root, commonDir, rest))
			if err != nil || !info.IsDir() {
				return nil, fmt.Errorf("%s: %s %q resolves to %s, which is not a directory", manifest, name, spec, resolved)
			}
			found[rest] = true
		}
	}
	return sortedKeys(found), nil
}

// releaseComponents lists the release-please package directories.
func releaseComponents(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, releaseConfig))
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", releaseConfig, err)
	}
	var dirs []string
	for dir := range cfg.Packages {
		dirs = append(dirs, path.Clean(dir))
	}
	return dirs, nil
}

// owningComponent is the deepest component directory containing dir, or "".
// A root-level package (".") would own everything and is never returned: a
// lock at the repository root is in no component's release path.
func owningComponent(components []string, dir string) string {
	best := ""
	for _, c := range components {
		if c == "." {
			continue
		}
		if (dir == c || strings.HasPrefix(dir, c+"/")) && len(c) > len(best) {
			best = c
		}
	}
	return best
}

// existingLocks lists the lock files in the tree (tracked, or untracked and
// not ignored).
func existingLocks(root string) ([]string, error) {
	return listFiles(root, lockName)
}

// listFiles lists files named name anywhere in the working tree that git
// tracks or would track: ignored files (node_modules, build output) are out by
// construction, without a second ignore list here.
func listFiles(root, name string) ([]string, error) {
	out, err := git(root, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard",
		"--", ":(glob)**/"+name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		// --cached and --others overlap for nothing, but a deleted-but-tracked
		// file is listed while absent from disk; it is not there to read.
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			continue
		}
		seen[p] = true
	}
	return sortedKeys(seen), nil
}

// TreeHash is the git tree id of common-ts/<pkg> as it stands in the working
// tree — staged into a throwaway index, so the real one is never touched.
// Equal to `git rev-parse HEAD:common-ts/<pkg>` whenever that content is what
// HEAD holds. See the package comment for why not HEAD itself.
func TreeHash(root, pkg string) (string, error) {
	dir := commonDir + "/" + pkg
	tmp, err := os.MkdirTemp("", "commonlock-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := git(root, env, "add", "--all", "--", dir); err != nil {
		return "", err
	}
	out, err := git(root, env, "write-tree", "--prefix="+dir+"/")
	if err != nil {
		return "", fmt.Errorf("%s has no files git would commit: %w", dir, err)
	}
	return strings.TrimSpace(out), nil
}

func git(root string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// describe says how a lock file's content differs from what it should be.
func describe(got, want string) string {
	parse := func(s string) map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(s, "\n") {
			if f := strings.Fields(line); len(f) == 2 && !strings.HasPrefix(line, "#") {
				m[f[0]] = f[1]
			}
		}
		return m
	}
	g, w := parse(got), parse(want)
	var parts []string
	for _, pkg := range sortedKeys(w) {
		switch {
		case g[pkg] == "":
			parts = append(parts, fmt.Sprintf("%s is missing (tree is %s)", pkg, w[pkg]))
		case g[pkg] != w[pkg]:
			parts = append(parts, fmt.Sprintf("%s is locked at %s but the tree is %s", pkg, g[pkg], w[pkg]))
		}
	}
	for _, pkg := range sortedKeys(g) {
		if _, ok := w[pkg]; !ok {
			parts = append(parts, fmt.Sprintf("%s is locked but no longer consumed", pkg))
		}
	}
	if len(parts) == 0 {
		return "not in the generated form"
	}
	return strings.Join(parts, "; ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
