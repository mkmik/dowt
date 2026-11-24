package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// review is where a PR or MR stands, said the same way for GitHub and GitLab.
type review struct {
	Ref, URL, Title string
	// State is open, draft, merged or closed.
	State string
	// Approval is approved, changes (requested) or pending; "" once it's in or dropped.
	Approval string
	// CI is running, failed, passed, canceled or manual; "" for no CI, or once it's in or dropped.
	CI string
}

// lookup is the branch checked out at dir, and its review: nil when the forge
// says it has none. Which forge comes from where origin points.
func lookup(dir string) (branch string, r *review, err error) {
	branch = gitOut(dir, "branch", "--show-current")
	if branch == "" {
		return "", nil, errors.New("no branch checked out")
	}
	host, project := parseRemote(gitOut(dir, "remote", "get-url", "origin"))
	switch {
	case host == "":
		return branch, nil, errors.New("no origin remote")
	case strings.Contains(host, "github"):
		r, err = ghLookup(dir)
	default:
		r, err = glabLookup(dir, host, project, cmp.Or(pushedAs(dir, branch), branch))
	}
	return branch, r, err
}

// pushedAs is the name origin knows branch by, which may be another, or "".
// Read off the config: once merged, origin deletes the branch and a fetch
// prunes its ref, and the name has to outlive both to find the review.
func pushedAs(dir, branch string) string {
	return gitOut(dir, "for-each-ref", "--format=%(upstream:lstrip=3)", "refs/heads/"+branch)
}

// gitOut is what git says, "" when it fails: some commands print their
// argument back on the way out (rev-parse), which is no answer.
func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// run runs a command in dir and returns its stdout, or an error with its stderr.
func run(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, errors.New(msg)
		}
	}
	return out, err
}

// parseRemote is the host and the project path of a git remote url: ssh
// (git@host:group/proj.git, ssh://git@host:22/group/proj) or https.
func parseRemote(u string) (host, project string) {
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		host, project, _ = strings.Cut(u, "/")
		host = host[strings.LastIndex(host, "@")+1:]
		host, _, _ = strings.Cut(host, ":")
		return host, project
	}
	host, project, ok := strings.Cut(u, ":")
	if !ok {
		return "", ""
	}
	return host[strings.LastIndex(host, "@")+1:], project
}

const ghFields = "number,url,title,state,isDraft,reviewDecision,statusCheckRollup"

// ghLookup asks gh for the pull request of the branch checked out at dir.
func ghLookup(dir string) (*review, error) {
	out, err := run(dir, "gh", "pr", "view", "--json", ghFields)
	if err != nil {
		if strings.Contains(err.Error(), "no pull requests found") {
			return nil, nil
		}
		return nil, err
	}
	return ghReview(out)
}

func ghReview(out []byte) (*review, error) {
	var pr struct {
		Number         int
		URL, Title     string
		State          string
		IsDraft        bool
		ReviewDecision string
		// A check run has a status and a conclusion; a commit status has a state.
		StatusCheckRollup []struct{ Status, Conclusion, State string }
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, err
	}
	r := &review{Ref: "#" + strconv.Itoa(pr.Number), URL: pr.URL, Title: pr.Title, State: strings.ToLower(pr.State)}
	if r.State != "open" {
		return r, nil
	}
	if pr.IsDraft {
		r.State = "draft"
	}
	r.Approval = map[string]string{"APPROVED": "approved", "CHANGES_REQUESTED": "changes"}[pr.ReviewDecision]
	if r.Approval == "" {
		r.Approval = "pending"
	}
	running, failed := false, false
	for _, c := range pr.StatusCheckRollup {
		switch {
		case c.Status != "" && c.Status != "COMPLETED", c.State == "PENDING", c.State == "EXPECTED":
			running = true
		case c.State == "FAILURE", c.State == "ERROR",
			c.Conclusion == "FAILURE", c.Conclusion == "TIMED_OUT", c.Conclusion == "CANCELLED",
			c.Conclusion == "ACTION_REQUIRED", c.Conclusion == "STARTUP_FAILURE":
			failed = true
		}
	}
	switch {
	case failed:
		r.CI = "failed"
	case running:
		r.CI = "running"
	case len(pr.StatusCheckRollup) > 0:
		r.CI = "passed"
	}
	return r, nil
}

// glabQuery is the latest merge request from branch, in one round trip.
const glabQuery = `query($project: ID!, $branch: String!) {
  project(fullPath: $project) {
    mergeRequests(sourceBranches: [$branch], sort: UPDATED_DESC, first: 1) {
      nodes { iid title webUrl state draft approved headPipeline { status } }
    }
  }
}`

// glabLookup asks glab for the latest merge request from branch.
func glabLookup(dir, host, project, branch string) (*review, error) {
	out, err := run(dir, "glab", "api", "graphql", "--hostname", host,
		"-f", "query="+glabQuery, "-f", "project="+project, "-f", "branch="+branch)
	if err != nil {
		return nil, err
	}
	return glabReview(out)
}

func glabReview(out []byte) (*review, error) {
	var resp struct {
		Data struct {
			Project *struct {
				MergeRequests struct {
					Nodes []struct {
						IID, Title, WebURL, State string
						Draft, Approved           bool
						HeadPipeline              *struct{ Status string }
					}
				}
			}
		}
		Errors []struct{ Message string }
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, errors.New(resp.Errors[0].Message)
	}
	if resp.Data.Project == nil || len(resp.Data.Project.MergeRequests.Nodes) == 0 {
		return nil, nil
	}
	mr := resp.Data.Project.MergeRequests.Nodes[0]
	r := &review{Ref: "!" + mr.IID, URL: mr.WebURL, Title: mr.Title, State: mr.State}
	switch mr.State {
	case "opened":
		r.State = "open"
	case "locked":
		r.State = "closed"
	}
	if r.State != "open" {
		return r, nil
	}
	if mr.Draft {
		r.State = "draft"
	}
	r.Approval = "pending"
	if mr.Approved {
		r.Approval = "approved"
	}
	if mr.HeadPipeline != nil {
		switch mr.HeadPipeline.Status {
		case "SUCCESS":
			r.CI = "passed"
		case "FAILED":
			r.CI = "failed"
		case "CANCELED", "CANCELING":
			r.CI = "canceled"
		case "MANUAL":
			r.CI = "manual"
		case "SKIPPED":
		default:
			r.CI = "running"
		}
	}
	return r, nil
}
