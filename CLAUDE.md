# dowt

dowt's panels are drawn by dense, which refuses any it doesn't accept. Before
merging, check them against dense, without building or installing dense:

- `go test ./...` writes each kind of panel to `testdata/panels.jsonl` (with
  `-update`, after a change to `view` that is meant).
- `<dense>/macos/A2UICheck/check.sh testdata/panels.jsonl` checks that file
  against the catalog of a dense checkout (`~/p/dense`, or the worktree of the
  dense change this one needs). CI does it against dense's main.

When the change needs one in dense, merge dense's first: dowt's CI checks
against dense's main. Merge once CI is green.
