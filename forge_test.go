package main

import "testing"

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
