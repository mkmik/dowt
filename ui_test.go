package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/panels.jsonl")

// TestPanels draws a panel in each state it can be in, as the createSurface dense gets, and
// compares them with testdata/panels.jsonl (go test -update rewrites it). dense checks that
// file against its catalog (its macos/A2UICheck/check.sh --dowt, and dowt's CI), so a panel
// dense would refuse is caught before it's merged, on either side.
func TestPanels(t *testing.T) {
	now := time.Now()
	// By Space name. Spaces on a worktree, named after it, have a ✓; the others don't.
	panels := []struct {
		name, dir string
		st        *status
	}{
		{"nodir", "", &status{}},
		{"looking", "/r", &status{}},
		{"none", "/r", &status{branch: "main", at: now, made: 2, note: "made Space x"}},
		{"err", "/r", &status{branch: "main", at: now, err: errors.New("gh: not logged in")}},
		{"open", "/r/.claude/worktrees/open", &status{branch: "b", at: now,
			review: &review{Ref: "#1", URL: "https://x/1", Title: "t", State: "open", Approval: "approved", CI: "failed"}}},
		{"merged", "/r/.claude/worktrees/merged", &status{branch: "b", at: now, checked: now,
			review: &review{Ref: "!2", URL: "https://x/2", Title: "t", State: "merged"}}},
		{"draft", "/r/.claude/worktrees/draft", &status{branch: "b", at: now, checked: now, unsafe: "uncommitted changes", note: "removing failed",
			review: &review{State: "draft", Approval: "changes", CI: "manual"}}},
	}
	d := newDowt(nil, "")
	d.start = start{Command: "claude", Run: true}
	var got bytes.Buffer
	for _, p := range panels {
		sp := space{}
		sp.Spec.WorkingDir = p.dir
		d.spaces[p.name] = sp
		d.status[p.dir] = p.st
		b, err := json.Marshal(d.create(prefix + p.name))
		if err != nil {
			t.Fatal(err)
		}
		got.Write(append(b, '\n'))
	}
	const file = "testdata/panels.jsonl"
	if *update {
		if err := os.WriteFile(file, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, _ := os.ReadFile(file)
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("panels differ from %s; check them with dense's check.sh, then go test -update", file)
	}
}

// The +'s command and checkbox are kept for the next dowt.
func TestStartKept(t *testing.T) {
	file := t.TempDir() + "/dowt/start.json"
	if got := loadStart(file); got != (start{Command: "claude"}) {
		t.Errorf("with none kept: %+v", got)
	}
	d := newDowt(nil, "")
	d.startFile = file
	want := start{Command: "codex --yolo", Run: true}
	d.setStart(want)
	if got := loadStart(file); got != want {
		t.Errorf("kept %+v, want %+v", got, want)
	}
}
