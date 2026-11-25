# dowt

A [dense](https://github.com/mkmik/dense) Extension that shows, next to each
Space, where its worktree's review stands, and makes new worktrees.

Every Space gets a small panel in the sidebar box named `details`:

- the branch checked out in the Space's working directory;
- its PR (GitHub) or MR (GitLab), if there is one: open, draft, merged or
  closed; approved or not; CI running, failed or passed; and its title,
  which opens it in the browser;
- a toolbar of two buttons, drawn like dense's own (grey symbols):
  - **+** opens a sheet that asks for a title for the new Space (left
    empty, the worktree's name). **Create** makes a new worktree of the
    Space's repo, with a random name like `dreamy-foraging-hickey`, at
    `<repo>/.claude/worktrees/<name>` on a new branch `worktree-<name>`
    started from the upstream of the main checkout's branch (usually
    `origin/main`), with copies of the ignored files `.worktreeinclude`
    names. Then it adds a Space for it, under the Space of the repo's main
    checkout, with a terminal tab open in it, and switches the window you
    clicked in to it. Under the title, a **Command** (`claude`, `codex`, …)
    and a checkbox, **Run in its terminal**: when it is on, the new terminal
    runs that command in your shell, as if you had typed it, and leaves you
    in the shell when it exits. dowt remembers both, for the next **+**, in
    `~/.local/state/dowt/start.json`.
  - **✓** ("done, clean it up") is on Spaces whose directory is such a
    worktree. It is green when removing it is safe: the branch is merged into
    `main` or `origin/main` (also squash-merged), nothing is uncommitted, and
    no Claude Code is working there. A click then removes the worktree, its
    branch and the Spaces on it, with their tabs and terminals, without
    asking. It stays grey when it isn't safe; a click then opens a sheet
    saying why, with **Remove anyway**. Safety is checked again every 30
    seconds, and once more by the removal itself.

Each Space also gets a badge in dense's sidebar, the same ones ccwt puts
there: `✓` merged (`±` if the worktree has uncommitted changes); while it is
open, `☐` waiting on review, `■` on a merge train or in the merge queue, `▣`
CI running, `☒` CI failed; `?` once the forge has failed to answer for 10
minutes. A Space with no review, or a closed one, has no badge. dowt
replaces only these glyphs, and leaves other badges on the Space alone.

To keep "merged" true to the forge, dowt fetches `main` (`master` where there
is no `main`) from `origin` every minute, in the repo of each worktree a Space
is on, and at once when a review comes back merged while its ✓ is still grey.

Worktrees, branches and locks are laid out as ccwt lays them out, so the two
work on each other's worktrees.

## How it works

dowt is a controller. It watches dennet's Spaces and Surfaces. For each
Space it keeps one Surface named `dowt-<space>`, of kind `ui`, in the
`details` box. It creates the missing ones (also one you closed) and deletes
the ones whose Space is gone or archived.

dense draws those Surfaces by connecting to dowt's Unix socket and speaking
A2UI 1.0. dowt asks `gh` or `glab` (picked from where `origin` points) about
each panel's branch at most every 2 minutes, 4 at a time.

## Run

    go install github.com/mkmik/dowt@latest
    dowt

It needs a dense whose catalog has `ToolbarButton`, dennet running, and `git`
and `gh`/`glab` on `PATH`; with `git-tree-merged` there too, it recognizes
more squash and rebase merges as merged. To
remove its panels, stop it and `dennet delete extension dowt`; otherwise
dennet removes them 10 minutes after dowt stops.

## Test

`go test ./...` also writes every kind of panel to `testdata/panels.jsonl`
(`go test -update` once a change to them is meant). dense's
`macos/A2UICheck/check.sh testdata/panels.jsonl` checks them against dense's
catalog in a minute or two, without building dense; CI checks them against
dense's main, and dense's CI checks dowt's main against each dense change.

To change what the panel shows, edit `view` in `ui.go`; for another forge,
`lookup` in `forge.go`; for worktrees, `worktree.go`.
