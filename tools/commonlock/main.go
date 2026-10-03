// Command commonlock keeps each common-ts consumer's lock file in step with the
// shared packages it builds against (docs/55 D10).
//
//	go -C tools/commonlock run . check    # fail, naming each stale lock file
//	go -C tools/commonlock run . update   # rewrite every lock from the tree
//
// (`go -C`, because tools/commonlock is its own module and the repository
// root has none; any directory inside the checkout works as the working
// directory, and -root overrides the `git rev-parse --show-toplevel` default.)
//
// # Why the lock exists
//
// release-please attributes a squash commit to components by the paths it
// touched. A commit that changes only common-ts/<pkg> touches no component, so
// it would release nothing, and the consumers would ship the new shared code
// at some later, unrelated release — each at a different time. A lock file
// inside each consumer's release path, carrying a hash of every common-ts
// package it consumes, turns "the shared code changed" into "this component
// changed". CI's `common-ts-lock` job runs `update`, commits the result to the
// PR branch and then runs `check`, which is a required status.
//
// # What a consumer is
//
// Any package.json (tracked, or untracked and not ignored; never under
// node_modules or common-ts itself) with a dependency whose spec is `file:`
// and whose path, resolved against that package.json's directory, is exactly
// common-ts/<pkg>. Its lock is <component>/common-ts.lock, where <component>
// is the release-please package (release-please-config.json) whose directory
// contains the package.json — gawk-admin/ui/package.json locks into
// gawk-admin/common-ts.lock. A consumer outside every component is an error:
// its lock could release nothing.
//
// # Which hash: the working tree, as git would commit it
//
// Each line is `<pkg> <tree hash>`, where the hash is the git tree object id
// of common-ts/<pkg> — the value `git rev-parse HEAD:common-ts/<pkg>` prints
// once that tree is committed. It is computed from the WORKING TREE rather
// than read from HEAD: the files are staged into a throwaway index
// (GIT_INDEX_FILE in a temp dir; the real index is never touched) with `git
// add -A`, which honours .gitignore and .gitattributes exactly as a commit
// would, and `git write-tree --prefix` names the subtree. On CI's clean
// checkout that IS HEAD's hash. Locally it means the natural order — edit the
// package, run `update`, commit both — produces a fresh lock, where a
// HEAD-based hash would have recorded the tree from BEFORE the edit and failed
// in CI. The one way to disagree with CI is to leave a non-ignored file
// uncommitted, which CI's check then reports.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// updateCommand is what a stale check tells the reader to run.
const updateCommand = "go -C tools/commonlock run . update"

func main() {
	fs := flag.NewFlagSet("commonlock", flag.ExitOnError)
	root := fs.String("root", "", "repository root (default: git rev-parse --show-toplevel)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: commonlock [-root DIR] check|update")
		fs.PrintDefaults()
	}
	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	if err := fs.Parse(os.Args[2:]); err != nil || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *root == "" {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, "commonlock: not inside a git checkout:", err)
			os.Exit(1)
		}
		*root = strings.TrimSpace(string(out))
	}

	var err error
	switch cmd {
	case "check":
		var stale []Stale
		stale, err = Check(*root)
		if err == nil && len(stale) > 0 {
			for _, s := range stale {
				fmt.Fprintf(os.Stderr, "::error file=%s::%s is stale: %s\n", s.Path, s.Path, s.Reason)
			}
			fmt.Fprintf(os.Stderr, "\n%d common-ts lock file(s) are stale (docs/55 D10). Run\n\n    %s\n\nand commit the result.\n", len(stale), updateCommand)
			os.Exit(1)
		}
		if err == nil {
			fmt.Println("commonlock: every common-ts lock file is fresh")
		}
	case "update":
		var changed []string
		changed, err = Update(*root)
		if err == nil {
			if len(changed) == 0 {
				fmt.Println("commonlock: every common-ts lock file is already fresh")
			}
			for _, p := range changed {
				fmt.Println("commonlock: wrote", p)
			}
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "commonlock:", err)
		os.Exit(1)
	}
}
