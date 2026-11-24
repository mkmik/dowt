package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// initRepo makes a repo with one commit on main, in a fresh directory that is
// also the cwd, and returns its path as git spells it.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := realpath(t.TempDir())
	t.Chdir(repo)
	gitT(t, "init", "-b", "main")
	gitT(t, "config", "core.hooksPath", "/dev/null") // not the user's own hooks
	gitT(t, "commit", "--allow-empty", "-m", "init")
	return repo
}

func gitT(t *testing.T, args ...string) string {
	t.Helper()
	args = append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitWork commits a file holding msg in the worktree at path: content, as
// an empty commit leaves the tree as main's, which is (rightly) merged.
func commitWork(t *testing.T, path, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, "f"), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, "-C", path, "add", "f")
	gitT(t, "-C", path, "commit", "-m", msg)
}

func mustMake(t *testing.T, dir string) (path, name string) {
	t.Helper()
	path, err := makeWorktree(dir)
	if err != nil {
		t.Fatal(err)
	}
	return path, filepath.Base(path)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// A new worktree is under the main checkout, on its own branch, locked as
// ours; and made from inside a worktree, it is beside it, not in it.
func TestMakeWorktree(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	if want := filepath.Join(repo, ".claude", "worktrees", name); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got := gitT(t, "-C", path, "branch", "--show-current"); got != branchPrefix+name {
		t.Errorf("branch = %q, want %q", got, branchPrefix+name)
	}
	wts, _ := listWorktrees(repo)
	if i := slices.IndexFunc(wts, func(w worktree) bool { return w.path == path }); i < 0 || wts[i].lock != lockReason {
		t.Errorf("worktrees = %+v, want %s locked %q", wts, path, lockReason)
	}
	beside, _ := mustMake(t, path)
	if filepath.Dir(beside) != filepath.Dir(path) {
		t.Errorf("made from a worktree: %q, want it beside %q", beside, path)
	}
}

// The new branch starts at the upstream of the main checkout's branch,
// which is ahead of it, and doesn't track it.
func TestMakeWorktreeFromUpstream(t *testing.T) {
	repo := initRepo(t)
	gitT(t, "commit", "--allow-empty", "-m", "upstream")
	gitT(t, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitT(t, "reset", "-q", "--hard", "HEAD~")
	gitT(t, "remote", "add", "origin", repo)
	gitT(t, "branch", "--set-upstream-to=origin/main")

	path, _ := mustMake(t, repo)
	if got, want := gitT(t, "-C", path, "rev-parse", "HEAD"), gitT(t, "rev-parse", "origin/main"); got != want {
		t.Errorf("worktree HEAD = %q, want origin/main %q", got, want)
	}
	if out, err := exec.Command("git", "-C", path, "rev-parse", "@{u}").CombinedOutput(); err == nil {
		t.Errorf("new branch tracks %s, want no upstream", out)
	}
}

// .worktreeinclude copies what it names and git ignores, with its mode; not
// what git tracks or doesn't ignore.
func TestMakeWorktreeCopiesInclude(t *testing.T) {
	repo := initRepo(t)
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755)
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".env\nsecrets/\n", 0o644)
	write(includeFile, ".env\nsecrets/**\n*.md\nmissing.env\n", 0o644)
	write(".env", "TOKEN=1", 0o600)     // ignored and named: copied, still 0600
	write("secrets/s.json", "S", 0o644) // from inside an ignored dir too
	write("notes.md", "scratch", 0o644) // named, not ignored: left
	write("README.md", "doc", 0o644)    // named, tracked: checked out, not copied
	gitT(t, "add", "README.md", ".gitignore")
	gitT(t, "commit", "-m", "readme")

	path, _ := mustMake(t, repo)
	for rel, want := range map[string]string{".env": "TOKEN=1", "secrets/s.json": "S", "notes.md": "", "README.md": "doc"} {
		got, err := os.ReadFile(filepath.Join(path, rel))
		switch {
		case want == "" && err == nil:
			t.Errorf("%s was copied, want it absent", rel)
		case want != "" && string(got) != want:
			t.Errorf("%s = %q (%v), want %q", rel, got, err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(path, ".env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf(".env: %v, %v; want mode 0600", fi, err)
	}
}

// An unmerged branch is unsafe and the worktree stays; deleting it anyway
// takes worktree and branch.
func TestRemoveUnmerged(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	commitWork(t, path, "ahead")

	if err := unsafeToRemove(repo, name, nil); err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Errorf("unmerged: %v, want not merged", err)
	}
	if err := deleteWorktree(repo, name); err != nil {
		t.Fatal(err)
	}
	if exists(path) || branchExists(repo, branchPrefix+name) {
		t.Error("forced removal left the worktree or its branch")
	}
}

// A clean, merged worktree is safe, and goes with its branch.
func TestRemoveMerged(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Fatalf("fresh worktree: %v", err)
	}
	if err := deleteWorktree(repo, name); err != nil {
		t.Fatal(err)
	}
	if exists(path) || branchExists(repo, branchPrefix+name) {
		t.Error("removal left the worktree or its branch")
	}
	if err := deleteWorktree(repo, name); err != nil {
		t.Errorf("removing it again: %v", err)
	}
}

// Uncommitted work, untracked files too, is unsafe: the removal is forced.
func TestRemoveDirty(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	os.WriteFile(filepath.Join(path, "scratch"), nil, 0o600)
	if err := unsafeToRemove(repo, name, nil); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("dirty: %v, want uncommitted changes", err)
	}
}

// An agent at work in the worktree, or a directory in it, is unsafe; one
// elsewhere isn't.
func TestRemoveAgentWorking(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	if err := unsafeToRemove(repo, name, []string{filepath.Join(path, "internal")}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("agent working: %v, want refusal", err)
	}
	if err := unsafeToRemove(repo, name, []string{repo, path + "-other"}); err != nil {
		t.Errorf("agents elsewhere: %v", err)
	}
}

// A worktree switched off the branch it was made on is judged by the one it
// has checked out, and only that one is deleted.
func TestRemoveSwitchedBranch(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	commitWork(t, path, "stale")
	gitT(t, "-C", path, "switch", "-c", "landed", "main")

	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Fatalf("on a merged branch: %v", err)
	}
	if err := deleteWorktree(repo, name); err != nil {
		t.Fatal(err)
	}
	if branchExists(repo, "landed") {
		t.Error("the checked-out branch survived")
	}
	if !branchExists(repo, branchPrefix+name) {
		t.Error("the unmerged branch it started on was deleted")
	}
}

// Contained in main, with a stale origin/<branch> still at the pre-squash
// commits: `branch -d` would refuse after the worktree had gone.
func TestRemoveSquashMerged(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	branch := branchPrefix + name
	gitT(t, "-C", path, "commit", "--allow-empty", "-m", "work")
	gitT(t, "remote", "add", "origin", repo)
	gitT(t, "update-ref", "refs/remotes/origin/"+branch, "refs/heads/"+branch)
	gitT(t, "config", "branch."+branch+".remote", "origin")
	gitT(t, "config", "branch."+branch+".merge", "refs/heads/"+branch)
	gitT(t, "commit", "--allow-empty", "-m", "squashed work (#1)")
	gitT(t, "-C", path, "reset", "--hard", "main")

	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Fatal(err)
	}
	if err := deleteWorktree(repo, name); err != nil {
		t.Fatal(err)
	}
	if exists(path) || branchExists(repo, branch) {
		t.Error("removal left the worktree or its branch")
	}
}

// A real squash merge: the branch's tip is no ancestor of main, but its
// content is on it. git-tree-merged sees the same tree.
func TestRemoveSquashMergedNotAncestor(t *testing.T) {
	if !haveTreeMerged() {
		t.Skip("git-tree-merged not installed")
	}
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	commitWork(t, path, "work")
	gitT(t, "merge", "--squash", branchPrefix+name)
	gitT(t, "commit", "-m", "squashed work (#1)")
	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Error(err)
	}
}

// A squash main edited over since: the tree walk conflicts, the squash
// commit's patch-id still matches the branch's diff.
func TestRemoveSquashMergedThenEdited(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	commitWork(t, path, "work")
	gitT(t, "merge", "--squash", branchPrefix+name)
	gitT(t, "commit", "-m", "squashed work (#1)")
	commitWork(t, repo, "later work (#2)")
	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Error(err)
	}
	// And an edited squash isn't one.
	path, name = mustMake(t, repo)
	commitWork(t, path, "more")
	gitT(t, "merge", "--squash", branchPrefix+name)
	os.WriteFile(filepath.Join(repo, "g"), []byte("extra"), 0o600)
	gitT(t, "add", "g")
	gitT(t, "commit", "-m", "squashed and edited (#3)")
	if !haveTreeMerged() && unsafeToRemove(repo, name, nil) == nil {
		t.Error("an edited squash read as merged")
	}
}

// Merged into origin/main, though main wasn't pulled.
func TestRemoveMergedUpstream(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	gitT(t, "-C", path, "commit", "--allow-empty", "-m", "work")
	gitT(t, "update-ref", "refs/remotes/origin/main", "refs/heads/"+branchPrefix+name)
	if err := unsafeToRemove(repo, name, nil); err != nil {
		t.Error(err)
	}
}

// A lock set by hand is honoured; a worktree of ours whose directory is gone
// is pruned though locked.
func TestRemoveLocks(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	gitT(t, "worktree", "unlock", path)
	gitT(t, "worktree", "lock", "--reason", "mine", path)
	if err := deleteWorktree(repo, name); err == nil || !exists(path) {
		t.Errorf("hand-locked: %v, want refusal and the worktree kept", err)
	}

	path, name = mustMake(t, repo)
	os.RemoveAll(path)
	if err := deleteWorktree(repo, name); err != nil {
		t.Fatal(err)
	}
	wts, _ := listWorktrees(repo)
	if slices.ContainsFunc(wts, func(w worktree) bool { return w.path == path }) {
		t.Errorf("gone worktree still registered: %+v", wts)
	}
}

func TestWorking(t *testing.T) {
	for title, want := range map[string]bool{"✳ Fix the bug": false, "◐ Work": true, "⠂ Work": true, "main": false, "": false} {
		if working(title) != want {
			t.Errorf("working(%q) = %v", title, !want)
		}
	}
}

// fakeDennet serves lists of objects, each a JSON array, and records deletes.
type fakeDennet struct {
	lists   map[string]string
	mu      sync.Mutex
	deletes []string
}

func (f *fakeDennet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	res := r.URL.Path[strings.LastIndex(r.URL.Path, "namespaces/default/")+len("namespaces/default/"):]
	if r.Method == http.MethodDelete {
		f.mu.Lock()
		f.deletes = append(f.deletes, res)
		f.mu.Unlock()
		return
	}
	w.Write([]byte(`{"items":` + f.lists[res] + `}`))
}

func newFakeDowt(t *testing.T, f *fakeDennet) *dowt {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newDowt(&dennet{ns: "default", api: srv.URL + "/apis/den.mkm.pub/v1alpha1/namespaces/default/", c: srv.Client()}, "")
}

// The ✓: with an agent at work in the worktree it removes nothing and says
// why; Remove anyway closes the Space on it, its tabs and their dens, and
// removes the worktree and its branch.
func TestRemoveWorktreeAction(t *testing.T) {
	repo := initRepo(t)
	path, name := mustMake(t, repo)
	spaces := `[{"metadata":{"name":"wt"},"spec":{"workingDir":` + quote(path) + `}},{"metadata":{"name":"repo"},"spec":{"workingDir":` + quote(repo) + `}}]`
	f := &fakeDennet{lists: map[string]string{
		"spaces":   spaces,
		"surfaces": `[{"metadata":{"name":"wt-1"},"spec":{"space":"wt","terminal":{"den":"wt-1"}}},{"metadata":{"name":"dowt-wt"},"spec":{"space":"wt","ui":{"extension":"dowt"}}},{"metadata":{"name":"repo-1"},"spec":{"space":"repo","terminal":{"den":"repo-1"}}}]`,
		"dens":     `[{"metadata":{"name":"wt-1"},"status":{"phase":"Running","title":"◐ busy","cwd":` + quote(path) + `}}]`,
	}}
	d := newFakeDowt(t, f)
	var sp space
	json.Unmarshal([]byte(`{"metadata":{"name":"wt"},"spec":{"workingDir":`+quote(path)+`}}`), &sp)
	d.spaces = map[string]space{"wt": sp}

	d.removeWorktree(prefix+"wt", false)
	if st := d.status[path]; st == nil || !strings.Contains(st.unsafe, "agent") || !exists(path) || f.deletes != nil {
		t.Fatalf("with an agent at work: status %+v, deletes %v; want refused", st, f.deletes)
	}

	d.removeWorktree(prefix+"wt", true)
	if exists(path) || branchExists(repo, branchPrefix+name) {
		t.Error("Remove anyway left the worktree or its branch")
	}
	if want := []string{"spaces/wt", "surfaces/wt-1", "surfaces/dowt-wt", "dens/wt-1"}; !slices.Equal(f.deletes, want) {
		t.Errorf("deletes = %v, want %v", f.deletes, want)
	}
	if st := d.status[path]; st.note != "removed "+name {
		t.Errorf("note = %q", st.note)
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// fetchMain brings origin's main into origin/main (master, in a repo without
// main), and leaves the local branch alone.
func TestFetchMain(t *testing.T) {
	for _, base := range []string{"main", "master"} {
		t.Run(base, func(t *testing.T) {
			remote := initRepo(t)
			gitT(t, "branch", "-m", base)
			repo := realpath(t.TempDir())
			gitT(t, "clone", "-q", remote, repo)
			gitT(t, "-C", remote, "commit", "--allow-empty", "-m", "landed")

			fetchMain(repo)
			if got, want := gitT(t, "-C", repo, "rev-parse", "origin/"+base), gitT(t, "-C", remote, "rev-parse", base); got != want {
				t.Errorf("origin/%s = %s, want %s", base, got, want)
			}
			if gitT(t, "-C", repo, "rev-parse", base) == gitT(t, "-C", remote, "rev-parse", base) {
				t.Errorf("the local %s moved", base)
			}
		})
	}
	fetchMain(t.TempDir()) // not a repo: nothing, and no hang
}

// The repos fetched are those of the worktrees live Spaces are on, once each.
func TestRoots(t *testing.T) {
	d := newDowt(nil, "")
	add := func(name, dir string, archived bool) {
		var sp space
		sp.Spec.WorkingDir, sp.Spec.Archived = dir, archived
		d.spaces[name] = sp
	}
	add("a", "/r/.claude/worktrees/a", false)
	add("b", "/r/.claude/worktrees/b", false)
	add("r", "/r", false)
	add("old", "/old/.claude/worktrees/x", true)
	add("s", "/s/.claude/worktrees/y", false)
	got := d.roots()
	slices.Sort(got)
	if want := []string{"/r", "/s"}; !slices.Equal(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}
