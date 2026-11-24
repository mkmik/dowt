package main

// What dowt used to ask ccwt to do: make a worktree, say whether removing one
// is safe, and remove it. The layout (<repo>/.claude/worktrees/<name>), the
// branch names (worktree-<name>) and the lock are ccwt's, so ccwt and dowt
// still agree on each other's worktrees.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// branchPrefix goes in front of a worktree's name to make its branch.
	branchPrefix = "worktree-"
	// lockReason is what a worktree we make is locked with, so `git worktree
	// prune` never reclaims one whose directory is briefly away, and so a
	// removal can tell our lock from one set by hand.
	lockReason = "ccwt"
	// includeFile names, in .gitignore syntax, the ignored files (.env and
	// such) a new worktree starts with a copy of. Claude Code reads it too.
	includeFile = ".worktreeinclude"
)

// git runs git in dir and returns its stdout, or an error with its stderr.
func git(dir string, args ...string) (string, error) {
	out, err := run(dir, "git", args...)
	return string(out), err
}

// makeWorktree makes a new worktree, on a new branch, of the repo dir is in
// (the main checkout's, when dir is one of its worktrees), and returns its path.
// The branch starts from the upstream of the main checkout's branch
// (origin/main, usually), which is fetched more often than main is pulled, and
// doesn't track it. No upstream: from HEAD.
func makeWorktree(dir string) (string, error) {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(top)
	if r, wt := worktreeOf(root); wt != "" {
		root = r
	}
	name := worktreeName()
	path := filepath.Join(root, ".claude", "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	args := []string{"worktree", "add", "--lock", "--reason", lockReason, "-b", branchPrefix + name, path}
	if up, err := git(root, "rev-parse", "--verify", "--quiet", "@{u}"); err == nil {
		args = append(args, strings.TrimSpace(up)) // a commit, not a ref: no tracking
	}
	if _, err := git(root, args...); err != nil {
		return "", err
	}
	// The worktree is there and usable without them.
	if err := copyInclude(root, path); err != nil {
		log.Printf("%s: %s: %v", path, includeFile, err)
	}
	return path, nil
}

// copyInclude copies into the worktree at path the files of root that
// .worktreeinclude names and git ignores: tracked ones are checked out
// already, and an untracked one nobody ignores is a stray, not local config.
func copyInclude(root, path string) error {
	inc := filepath.Join(root, includeFile)
	if _, err := os.Stat(inc); err != nil {
		return nil
	}
	// Two runs: git unions the exclude sources it's given, so both in one
	// would list every ignored file.
	ignored, err := lsIgnored(root, "--exclude-standard")
	if err != nil {
		return err
	}
	wanted, err := lsIgnored(root, "--exclude-from="+inc)
	if err != nil {
		return err
	}
	for _, rel := range wanted {
		if !slices.Contains(ignored, rel) {
			continue
		}
		if err := copyFile(filepath.Join(root, rel), filepath.Join(path, rel)); err != nil {
			return err
		}
	}
	return nil
}

// lsIgnored is the untracked files of dir that exclude matches, relative to dir.
func lsIgnored(dir, exclude string) ([]string, error) {
	out, err := git(dir, "ls-files", "-z", "--others", "--ignored", exclude)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// copyFile copies src to dst with its mode, so a 0600 secret stays 0600.
// ponytail: whole file in memory; these are .env-sized.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, fi.Mode().Perm())
}

// unsafeToRemove says why removing the worktree name of the repo at root
// would lose something, or nil when it wouldn't: its branch isn't merged, it
// has uncommitted changes, or an agent is at work in it, busy being where
// agents are at work. Asked of git every time, never of a cache: it gates a
// deletion.
func unsafeToRemove(root, name string, busy []string) error {
	root = realpath(root)
	path := filepath.Join(root, ".claude", "worktrees", name)
	// A branch that isn't there (made by hand, deleted) has nothing to lose.
	if branch := worktreeBranch(root, name); branchExists(root, branch) && !merged(root, branch) {
		return fmt.Errorf("%s is not merged", branch)
	}
	// The removal is forced, so uncommitted work would be gone for good.
	if out, err := git(path, "status", "--porcelain"); err == nil && out != "" {
		return fmt.Errorf("%s has uncommitted changes", name)
	}
	// The one git can't see: a branch an agent made a minute ago and hasn't
	// committed to is both merged and clean.
	for _, cwd := range busy {
		if cwd == path || strings.HasPrefix(cwd, path+string(filepath.Separator)) {
			return fmt.Errorf("%s has an agent working in it", name)
		}
	}
	return nil
}

// deleteWorktree removes the worktree name of the repo at root and its branch,
// whatever is in them: the safety check is the caller's. What is gone
// already is fine, so a removal that stopped halfway can be done again. A
// worktree locked by hand, with a reason not ours, is refused by git.
func deleteWorktree(root, name string) error {
	root = realpath(root)
	path := filepath.Join(root, ".claude", "worktrees", name)
	branch := worktreeBranch(root, name) // while there is a worktree to read it off
	wts, _ := listWorktrees(root)
	for _, wt := range wts {
		// Ours, so prune can reclaim it, or the one going (whose remove would refuse).
		if gone(wt.path) || wt.path == path {
			if wt.lock == lockReason {
				git(root, "worktree", "unlock", wt.path)
			}
		}
	}
	if _, err := os.Stat(path); err == nil {
		if _, err := git(root, "worktree", "remove", "--force", path); err != nil && !strings.Contains(err.Error(), "is not a working tree") {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := git(root, "worktree", "prune"); err != nil {
		return err
	}
	// -D, not -d: -d asks "merged into its upstream?", and a squash-merged
	// branch with a stale origin/<branch> would refuse, after its worktree went.
	if _, err := git(root, "branch", "-D", branch); err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	return nil
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// realpath is path with its symlinks resolved, as git reports paths (macOS's
// /var is /private/var), or path when it can't be.
func realpath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

type worktree struct{ path, branch, lock string }

// listWorktrees is `git worktree list`: the main checkout first, then the
// linked ones, each with its branch ("" when detached) and lock reason.
func listWorktrees(dir string) ([]worktree, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []worktree
	for entry := range strings.SplitSeq(out, "\n\n") {
		var wt worktree
		for line := range strings.SplitSeq(entry, "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "worktree":
				wt.path = v
			case "branch":
				wt.branch = strings.TrimPrefix(v, "refs/heads/")
			case "locked":
				wt.lock = v
			}
		}
		if wt.path != "" {
			wts = append(wts, wt)
		}
	}
	return wts, nil
}

// worktreeBranch is the branch the worktree name has checked out, or, with
// none to read (detached, gone), the one it was made on. Not that one alone:
// a worktree switched to another branch, merged under that name, would be
// judged by the stale one it started on.
func worktreeBranch(root, name string) string {
	path := filepath.Join(root, ".claude", "worktrees", name)
	wts, _ := listWorktrees(root)
	for _, wt := range wts {
		if wt.path == path && wt.branch != "" {
			return wt.branch
		}
	}
	return branchPrefix + name
}

func branchExists(dir, branch string) bool {
	_, err := git(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// mainRefs is where a branch could have landed: main (master without one),
// local and origin's, those of the two there are. Merged into origin/main is
// safe though nobody pulled, merged locally is though nobody pushed.
func mainRefs(dir string) []string {
	for _, base := range []string{"main", "master"} {
		var refs []string
		for _, ref := range []string{base, "origin/" + base} {
			if _, err := git(dir, "rev-parse", "--verify", "--quiet", ref); err == nil {
				refs = append(refs, ref)
			}
		}
		if len(refs) > 0 {
			return refs
		}
	}
	return nil
}

// haveTreeMerged is whether the git-tree-merged plugin is there: an unknown
// git command exits 1, which is also its "not merged".
var haveTreeMerged = sync.OnceValue(func() bool {
	_, err := exec.LookPath("git-tree-merged")
	return err == nil
})

// merged is whether branch's work is on main already. A git failure is no:
// the answer gates a deletion. Ancestry first, as it's cheap; then, as squash
// and rebase rewrite commits, the same tree (with git-tree-merged); then a
// commit on main carrying the branch's whole diff (a squash main has edited
// over since, which the tree walk can't replay).
//
// ponytail: one `git log -p` walk of main per unmerged branch, unbounded; cap
// it if a huge repo makes the check slow.
func merged(dir, branch string) bool {
	for _, ref := range mainRefs(dir) {
		if _, err := git(dir, "merge-base", "--is-ancestor", branch, ref); err == nil {
			return true
		}
		if haveTreeMerged() {
			if _, err := git(dir, "tree-merged", branch, "--onto", ref, "-q"); err == nil {
				return true
			}
		}
		if squashed(dir, branch, ref) {
			return true
		}
	}
	return false
}

// squashed is whether a commit on ref since branch forked off it has
// branch's whole diff, by patch-id. An edited squash, or a branch with no
// diff, is no.
func squashed(dir, branch, ref string) bool {
	base, err := git(dir, "merge-base", branch, ref)
	if err != nil {
		return false
	}
	base = strings.TrimSpace(base)
	want := patchIDs(dir, "diff", base, branch)
	return len(want) == 1 && slices.Contains(patchIDs(dir, "log", "-p", base+".."+ref), want[0])
}

// patchIDs is the patch-id of each patch git args prints, piped straight
// into patch-id: `log -p` of main can run to tens of megabytes.
func patchIDs(dir string, args ...string) []string {
	src := exec.Command("git", args...)
	src.Dir = dir
	patches, err := src.StdoutPipe()
	if err != nil || src.Start() != nil {
		return nil
	}
	cmd := exec.Command("git", "patch-id", "--stable")
	cmd.Dir, cmd.Stdin = dir, patches
	out, err := cmd.Output()
	if src.Wait() != nil || err != nil {
		return nil
	}
	var ids []string
	for line := range strings.Lines(string(out)) {
		if id, _, ok := strings.Cut(line, " "); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// fetchMain fetches origin's main (master, where there's no main) into
// origin/main. Just that branch: a bare fetch walks every ref the remote has.
// Failing (offline, no origin) leaves origin/main where it was, which only
// keeps a ✓ grey, so it's dropped. Never prompts, and gives up after
// fetchTimeout: an ssh that never answers mustn't hold up the next.
func fetchMain(root string) {
	refs := mainRefs(root)
	if len(refs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	// origin main, not a refspec: the clone's own refspec moves origin/main,
	// and won't force it past a rewritten history.
	cmd := exec.CommandContext(ctx, "git", "fetch", "--quiet", "origin", strings.TrimPrefix(refs[0], "origin/"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Run()
}

// den is the part of a Den dowt reads.
type den struct {
	Status struct {
		Phase string `json:"phase"`
		Title string `json:"title"`
		CWD   string `json:"cwd"`
	} `json:"status"`
}

// busyCwds is where agents are at work: the cwd of each running Den whose
// title Claude Code leads with its working spinner. A dennet that can't say
// is an error, not nobody working: it gates a deletion.
//
// ponytail: Claude Code only, and blind to one blocked on a question; a hook
// that tells dennet is the way to both.
func (dn *dennet) busyCwds() ([]string, error) {
	dens, err := list[den](dn, "dens")
	var cwds []string
	for _, d := range dens {
		if d.Status.Phase == "Running" && working(d.Status.Title) && d.Status.CWD != "" {
			cwds = append(cwds, d.Status.CWD)
		}
	}
	return cwds, err
}

// working is whether a terminal title starts with Claude Code's working
// spinner: a half circle, or braille dots. ✳ is it waiting for you.
func working(title string) bool {
	r, _ := utf8.DecodeRuneInString(title)
	return strings.ContainsRune("◐◓◑◒", r) || r >= 0x2800 && r <= 0x28ff
}

// closeSpaces deletes the Spaces on any of dirs, their Surfaces and the Dens
// those show: dennet deletes nothing for us. What is gone already is fine.
func (dn *dennet) closeSpaces(dirs ...string) error {
	spaces, err := list[space](dn, "spaces")
	if err != nil {
		return err
	}
	surfaces, err := list[surface](dn, "surfaces")
	if err != nil {
		return err
	}
	var paths, dens []string
	for _, sp := range spaces {
		if !slices.Contains(dirs, sp.Spec.WorkingDir) {
			continue
		}
		paths = append(paths, "spaces/"+sp.Metadata.Name)
		for _, s := range surfaces {
			if s.Spec.Space == sp.Metadata.Name {
				paths = append(paths, "surfaces/"+s.Metadata.Name)
				if s.Spec.Terminal != nil && s.Spec.Terminal.Den != "" {
					dens = append(dens, "dens/"+s.Spec.Terminal.Den)
				}
			}
		}
	}
	for _, p := range append(paths, dens...) {
		if err := dn.do(http.MethodDelete, p, nil, nil); err != nil && !isCode(err, http.StatusNotFound) {
			return err
		}
	}
	return nil
}
