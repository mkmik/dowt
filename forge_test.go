package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	for in, want := range map[string][2]string{
		"git@github.com:mkmik/dowt.git":                        {"github.com", "mkmik/dowt"},
		"https://github.com/mkmik/dowt":                        {"github.com", "mkmik/dowt"},
		"ssh://git@gitlab.example.com:12051/a/b/c.git":         {"gitlab.example.com", "a/b/c"},
		"https://oauth2:tok@gitlab.example.com/group/proj.git": {"gitlab.example.com", "group/proj"},
		"": {"", ""},
	} {
		if h, p := parseRemote(in); h != want[0] || p != want[1] {
			t.Errorf("parseRemote(%q) = %q, %q; want %q", in, h, p, want)
		}
	}
}

func TestGHReview(t *testing.T) {
	pr := func(fields, rollup string) string {
		return `{"data":{"repository":{"pullRequests":{"nodes":[{` + fields +
			`,"commits":{"nodes":[{"commit":{"statusCheckRollup":` + rollup + `}}]}}]}}}}`
	}
	for in, want := range map[string]*review{
		`{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`:                                           nil,
		pr(`"number":43,"state":"OPEN","reviewDecision":null`, `null`):                                    {Ref: "#43", State: "open", Approval: "pending"},
		pr(`"number":1,"state":"OPEN","isDraft":true,"reviewDecision":"APPROVED"`, `{"state":"PENDING"}`): {Ref: "#1", State: "draft", Approval: "approved", CI: "running"},
		pr(`"number":1,"state":"OPEN","reviewDecision":"CHANGES_REQUESTED"`, `{"state":"FAILURE"}`):       {Ref: "#1", State: "open", Approval: "changes", CI: "failed"},
		pr(`"number":1,"state":"OPEN"`, `{"state":"SUCCESS"}`):                                            {Ref: "#1", State: "open", Approval: "pending", CI: "passed"},
		pr(`"number":1,"state":"OPEN","isInMergeQueue":true`, `{"state":"EXPECTED"}`):                     {Ref: "#1", State: "open", Approval: "pending", CI: "running", Queued: true},
		pr(`"number":45,"state":"MERGED"`, `{"state":"PENDING"}`):                                         {Ref: "#45", State: "merged"},
	} {
		r, err := ghReview([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if (r == nil) != (want == nil) || r != nil && *r != *want {
			t.Errorf("ghReview(%s) = %+v; want %+v", in, r, want)
		}
	}
	if _, err := ghReview([]byte(`{"errors":[{"message":"nope"}]}`)); err == nil {
		t.Error("graphql errors should be an error")
	}
}

// The badges are ccwt's, for the same states.
func TestGlyph(t *testing.T) {
	for _, c := range []struct {
		r     *review
		err   error
		dirty bool
		want  string
	}{
		{nil, nil, false, ""},
		{nil, errors.New("down"), false, "?"},
		{&review{State: "closed"}, nil, false, ""},
		{&review{State: "merged"}, nil, false, "✓"},
		{&review{State: "merged"}, nil, true, "±"},
		{&review{State: "open", CI: "passed"}, nil, false, "☐"},
		{&review{State: "draft"}, nil, false, "☐"},
		{&review{State: "open", CI: "running"}, nil, false, "▣"},
		{&review{State: "open", CI: "failed"}, nil, false, "☒"},
		{&review{State: "open", CI: "running", Queued: true}, nil, false, "■"},
	} {
		if got := glyph(c.r, c.err, c.dirty); got != c.want {
			t.Errorf("glyph(%+v, %v, %v) = %q; want %q", c.r, c.err, c.dirty, got, c.want)
		}
	}
}

func TestGLabReview(t *testing.T) {
	for in, want := range map[string]*review{
		`{"data":{"project":{"mergeRequests":{"nodes":[]}}}}`: nil,
		`{"data":{"project":null}}`:                           nil,
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","approved":true,"headPipeline":{"status":"RUNNING"}}]}}}}`:                                                            {Ref: "!7", State: "open", Approval: "approved", CI: "running"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","draft":true,"headPipeline":{"status":"FAILED"}}]}}}}`:                                                                {Ref: "!7", State: "draft", Approval: "pending", CI: "failed"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"merged","headPipeline":{"status":"SUCCESS"}}]}}}}`:                                                                            {Ref: "!7", State: "merged"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","detailedMergeStatus":"CI_STILL_RUNNING","autoMergeStrategy":"merge_train","headPipeline":{"status":"RUNNING"}}]}}}}`: {Ref: "!7", State: "open", Approval: "pending", CI: "running", Queued: true},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","detailedMergeStatus":"NOT_APPROVED","autoMergeStrategy":"merge_train"}]}}}}`:                                         {Ref: "!7", State: "open", Approval: "pending"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","headPipeline":{"status":"WAITING_FOR_CALLBACK"}}]}}}}`:                                                               {Ref: "!7", State: "open", Approval: "pending", CI: "waiting for callback"},
	} {
		r, err := glabReview([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if (r == nil) != (want == nil) || r != nil && *r != *want {
			t.Errorf("glabReview(%s) = %+v; want %+v", in, r, want)
		}
	}
	if _, err := glabReview([]byte(`{"errors":[{"message":"nope"}]}`)); err == nil {
		t.Error("graphql errors should be an error")
	}
}

func TestWorktreeOf(t *testing.T) {
	for in, want := range map[string][2]string{
		"/r/.claude/worktrees/foo": {"/r", "foo"},
		"/r":                       {"", ""},
		"/r/worktrees/foo":         {"", ""},
		"":                         {"", ""},
	} {
		if root, name := worktreeOf(in); root != want[0] || name != want[1] {
			t.Errorf("worktreeOf(%q) = %q, %q; want %q", in, root, name, want)
		}
	}
}

// The name the branch was pushed under, still there once origin deleted it
// and a fetch pruned its ref, as after a merge.
func TestPushedAs(t *testing.T) {
	repo := initRepo(t)
	gitT(t, "remote", "add", "origin", repo)
	gitT(t, "update-ref", "refs/remotes/origin/feature/x", "HEAD")
	gitT(t, "switch", "-c", "local")
	gitT(t, "branch", "--set-upstream-to=origin/feature/x")
	if got := pushedAs(repo, "local"); got != "feature/x" {
		t.Errorf("pushedAs = %q, want feature/x", got)
	}
	gitT(t, "update-ref", "-d", "refs/remotes/origin/feature/x")
	if got := pushedAs(repo, "local"); got != "feature/x" {
		t.Errorf("upstream gone: pushedAs = %q, want feature/x", got)
	}
	if got := pushedAs(repo, "main"); got != "" {
		t.Errorf("no upstream: pushedAs = %q, want empty", got)
	}
	gitT(t, "remote", "add", "upstream", repo)
	gitT(t, "update-ref", "refs/remotes/upstream/other", "HEAD")
	gitT(t, "branch", "--set-upstream-to=upstream/other")
	if got := pushedAs(repo, "local"); got != "" {
		t.Errorf("tracks another remote: pushedAs = %q, want empty", got)
	}
}

// The branch being rebased is still the branch while HEAD is detached.
func TestCheckedOutBranch(t *testing.T) {
	repo := initRepo(t)
	gitT(t, "switch", "-c", "feature")
	if got := checkedOutBranch(repo); got != "feature" {
		t.Errorf("checkedOutBranch = %q, want feature", got)
	}
	gitT(t, "switch", "--detach")
	os.MkdirAll(filepath.Join(repo, ".git", "rebase-merge"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "rebase-merge", "head-name"), []byte("refs/heads/feature\n"), 0o644)
	if got := checkedOutBranch(repo); got != "feature" {
		t.Errorf("mid-rebase: checkedOutBranch = %q, want feature", got)
	}
}

// A second dowt is refused while the first holds the lock, and gets it once
// the first has let go.
func TestLockOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dowt.sock.lock")
	if err := lockOnce(path); err != nil {
		t.Fatal(err)
	}
	first := held
	if err := lockOnce(path); err == nil || !strings.Contains(err.Error(), "another dowt") {
		t.Errorf("second lock: %v, want refusal", err)
	}
	first.Close()
	if err := lockOnce(path); err != nil {
		t.Errorf("after the first let go: %v", err)
	}
	held.Close()
}
