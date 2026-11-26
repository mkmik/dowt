package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
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
	// Approvers is who approved it, as ccwt's APPROVED column has them: by
	// username, bar a GitLab access token's bot, whose username is a hash and
	// whose name ("ci[bot]") says who it is. Kept once it's in too.
	Approvers string
	// CI is running, failed, passed, canceled or manual; "" for no CI, or once it's in or dropped.
	CI string
	// Queued is an open one on a merge train (GitLab) or in the merge queue
	// (GitHub): the pipeline running is the train's, and nothing is on you.
	Queued bool
}

// noReview is lookup's error when there is nothing to ask the forge about, as
// against the forge failing to answer: no review, rather than an unknown one.
type noReview string

func (e noReview) Error() string { return string(e) }

// lookup is the branch checked out at dir, and its review: nil when the forge
// says it has none. Which forge comes from where origin points.
func lookup(dir string) (branch string, r *review, err error) {
	branch = checkedOutBranch(dir)
	if branch == "" {
		return "", nil, noReview("no branch checked out")
	}
	host, project := parseRemote(gitOut(dir, "remote", "get-url", "origin"))
	switch {
	case host == "":
		return branch, nil, noReview("no origin remote")
	case strings.Contains(host, "github"):
		r, err = ghLookup(dir, host, project, cmp.Or(pushedAs(dir, branch), branch))
	default:
		r, err = glabLookup(dir, host, project, cmp.Or(pushedAs(dir, branch), branch))
	}
	return branch, r, err
}

// checkedOutBranch is the branch at dir, also while a rebase has HEAD
// detached: then it is only written down in the rebase's head-name.
func checkedOutBranch(dir string) string {
	if b := gitOut(dir, "branch", "--show-current"); b != "" {
		return b
	}
	for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		if path := gitOut(dir, "rev-parse", "--path-format=absolute", "--git-path", f); path != "" {
			if b, err := os.ReadFile(path); err == nil {
				return strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/")
			}
		}
	}
	return ""
}

// pushedAs is the name origin knows branch by, which may be another, or "";
// a branch tracking another remote's is no name on origin. Read off the
// config: once merged, origin deletes the branch and a fetch prunes its ref,
// and the name has to outlive both to find the review.
func pushedAs(dir, branch string) string {
	up := gitOut(dir, "for-each-ref", "--format=%(upstream:remotename) %(upstream:lstrip=3)", "refs/heads/"+branch)
	if pushed, ok := strings.CutPrefix(up, "origin "); ok {
		return pushed
	}
	return ""
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

// ghQuery is the latest pull request from branch, in one round trip. Asked
// of graphql rather than with gh pr view, which can't say isInMergeQueue.
const ghQuery = `query($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $branch, orderBy: {field: UPDATED_AT, direction: DESC}, first: 1) {
      nodes {
        number url title state isDraft isInMergeQueue reviewDecision
        latestOpinionatedReviews(first: 100) { nodes { state author { login } } }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      }
    }
  }
}`

// ghLookup asks gh for the latest pull request from branch.
func ghLookup(dir, host, project, branch string) (*review, error) {
	owner, name, _ := strings.Cut(project, "/")
	out, err := run(dir, "gh", "api", "graphql", "--hostname", host,
		"-f", "query="+ghQuery, "-f", "owner="+owner, "-f", "name="+name, "-f", "branch="+branch)
	if err != nil {
		return nil, err
	}
	return ghReview(out)
}

func ghReview(out []byte) (*review, error) {
	var resp struct {
		Data struct {
			Repository *struct {
				PullRequests struct {
					Nodes []struct {
						Number                   int
						URL, Title, State        string
						IsDraft, IsInMergeQueue  bool
						ReviewDecision           string
						LatestOpinionatedReviews struct {
							Nodes []struct {
								State  string
								Author struct{ Login string }
							}
						}
						Commits struct {
							Nodes []struct {
								Commit struct {
									StatusCheckRollup *struct{ State string }
								}
							}
						}
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
	if resp.Data.Repository == nil || len(resp.Data.Repository.PullRequests.Nodes) == 0 {
		return nil, nil
	}
	pr := resp.Data.Repository.PullRequests.Nodes[0]
	r := &review{Ref: "#" + strconv.Itoa(pr.Number), URL: pr.URL, Title: pr.Title, State: strings.ToLower(pr.State)}
	// Whoever's latest say was an approval, which is what github counts: one
	// who approved and then asked for changes hasn't approved it.
	var names []string
	for _, rv := range pr.LatestOpinionatedReviews.Nodes {
		if rv.State == "APPROVED" {
			names = append(names, rv.Author.Login)
		}
	}
	r.Approvers = strings.Join(names, ", ")
	if r.State != "open" {
		return r, nil
	}
	if pr.IsDraft {
		r.State = "draft"
	}
	r.Queued = pr.IsInMergeQueue
	r.Approval = map[string]string{"APPROVED": "approved", "CHANGES_REQUESTED": "changes"}[pr.ReviewDecision]
	if r.Approval == "" {
		r.Approval = "pending"
	}
	// What all the checks came to, as ccwt reads it; none is no CI.
	if n := pr.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		switch n[0].Commit.StatusCheckRollup.State {
		case "SUCCESS":
			r.CI = "passed"
		case "PENDING", "EXPECTED": // expected: a required check that hasn't reported yet
			r.CI = "running"
		default: // FAILURE, ERROR
			r.CI = "failed"
		}
	}
	return r, nil
}

// glabQuery is the latest merge request from branch, in one round trip.
const glabQuery = `query($project: ID!, $branch: String!) {
  project(fullPath: $project) {
    mergeRequests(sourceBranches: [$branch], sort: UPDATED_DESC, first: 1) {
      nodes { iid title webUrl state draft approved approvedBy { nodes { username name } } detailedMergeStatus autoMergeStrategy headPipeline { status } }
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

// tokenBot is the username gitlab makes up for an access token's bot user.
var tokenBot = regexp.MustCompile(`^(group|project)_\d+_bot_`)

func glabReview(out []byte) (*review, error) {
	var resp struct {
		Data struct {
			Project *struct {
				MergeRequests struct {
					Nodes []struct {
						IID, Title, WebURL, State string
						Draft, Approved           bool
						ApprovedBy                struct {
							Nodes []struct{ Username, Name string }
						}
						DetailedMergeStatus string
						AutoMergeStrategy   string
						HeadPipeline        *struct{ Status string }
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
	var names []string
	for _, u := range mr.ApprovedBy.Nodes {
		if tokenBot.MatchString(u.Username) {
			u.Username = u.Name
		}
		names = append(names, u.Username)
	}
	r.Approvers = strings.Join(names, ", ")
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
	// On the train, or waiting on a pipeline to join it; unless gitlab has
	// a plainer answer, as ccwt's "merge train" status has it.
	switch mr.DetailedMergeStatus {
	case "MERGEABLE", "CONFLICT", "NOT_APPROVED", "DISCUSSIONS_NOT_RESOLVED":
	default:
		r.Queued = strings.Contains(mr.AutoMergeStrategy, "merge_train")
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
		case "RUNNING", "PENDING", "CREATED", "PREPARING", "SCHEDULED", "WAITING_FOR_RESOURCE":
			r.CI = "running"
		default: // waiting for callback, and whatever gitlab adds: said, not taken for running
			r.CI = strings.ToLower(strings.ReplaceAll(mr.HeadPipeline.Status, "_", " "))
		}
	}
	return r, nil
}
