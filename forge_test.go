package main

import (
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
	for in, want := range map[string]review{
		`{"number":43,"state":"OPEN","reviewDecision":"","statusCheckRollup":[]}`:                                                                                       {State: "open", Approval: "pending"},
		`{"number":1,"state":"OPEN","isDraft":true,"reviewDecision":"APPROVED","statusCheckRollup":[{"status":"IN_PROGRESS"}]}`:                                         {State: "draft", Approval: "approved", CI: "running"},
		`{"number":1,"state":"OPEN","reviewDecision":"CHANGES_REQUESTED","statusCheckRollup":[{"status":"IN_PROGRESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]}`: {State: "open", Approval: "changes", CI: "failed"},
		`{"number":1,"state":"OPEN","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"}]}`:                                           {State: "open", Approval: "pending", CI: "passed"},
		`{"number":1,"state":"OPEN","statusCheckRollup":[{"state":"PENDING"}]}`:                                                                                         {State: "open", Approval: "pending", CI: "running"},
		`{"number":45,"state":"MERGED","statusCheckRollup":[{"status":"QUEUED"}]}`:                                                                                      {State: "merged"},
	} {
		r, err := ghReview([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if r.State != want.State || r.Approval != want.Approval || r.CI != want.CI {
			t.Errorf("ghReview(%s) = %+v; want %+v", in, *r, want)
		}
	}
}

func TestGLabReview(t *testing.T) {
	for in, want := range map[string]*review{
		`{"data":{"project":{"mergeRequests":{"nodes":[]}}}}`: nil,
		`{"data":{"project":null}}`:                           nil,
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","approved":true,"headPipeline":{"status":"RUNNING"}}]}}}}`: {Ref: "!7", State: "open", Approval: "approved", CI: "running"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"opened","draft":true,"headPipeline":{"status":"FAILED"}}]}}}}`:     {Ref: "!7", State: "draft", Approval: "pending", CI: "failed"},
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","state":"merged","headPipeline":{"status":"SUCCESS"}}]}}}}`:                 {Ref: "!7", State: "merged"},
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
