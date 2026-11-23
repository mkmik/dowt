# dowt

A [dense](https://github.com/mkmik/dense) Extension that shows, next to each
Space, where its worktree's review stands, and makes new worktrees.

Every Space gets a small panel in the sidebar box named `details`:

- the branch checked out in the Space's working directory;
- its PR (GitHub) or MR (GitLab), if there is one: open, draft, merged or
  closed; approved or not; CI running, failed or passed; and its title,
  which opens it in the browser;
- a **+** button. It runs `ccwt new --force-create --path` in the Space's
  working directory, then adds a Space for the new worktree, under the
  Space of the repo's main checkout, with a terminal tab open in the new
  worktree. Starting an agent there is up to you.

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

It needs dennet running, and `git`, `gh`/`glab` and `ccwt` on `PATH`. To
remove its panels, stop it and `dennet delete extension dowt`; otherwise
dennet removes them 10 minutes after dowt stops.

To change what the panel shows, edit `view` in `ui.go`; for another forge,
`lookup` in `forge.go`.
