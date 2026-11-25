package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	denseCatalog = "https://dense.mkm.pub/a2ui/v1/catalog.json"
	// refreshEvery is how old a review may get before it is asked for again.
	refreshEvery = 2 * time.Minute
	// recheckEvery is how old the answer to "is removing it safe?" may get:
	// sooner than a review, as a worktree gets dirty and agents start and stop.
	recheckEvery = 30 * time.Second
	// lookups is how many gh/glab lookups may run at once.
	lookups = 4
	// fetchEvery is how often origin's main is fetched in the repos of the
	// worktrees the panels show, and fetchTimeout how long one fetch may take.
	fetchEvery   = time.Minute
	fetchTimeout = time.Minute
)

// dowt holds what the panels show and who is looking. Panels are keyed by
// Surface name, statuses by working directory, so Spaces on the same
// directory share one lookup.
type dowt struct {
	dn       *dennet
	endpoint string
	sem      chan struct{}

	mu     sync.Mutex
	spaces map[string]space             // by name
	status map[string]*status           // by working dir
	subs   map[*session]map[string]bool // surfaces each frontend shows

	// start is what the +'s sheet last ran in a new worktree's terminal,
	// kept in startFile (none when "") so the sheet offers it again.
	start     start
	startFile string
}

// start is the +'s command, and whether to run it in the new worktree's terminal.
type start struct {
	Command string `json:"command"`
	Run     bool   `json:"run"`
}

// loadStart is the start kept in file, or claude, not run, when there is none.
func loadStart(file string) start {
	s := start{Command: "claude"}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// setStart keeps s for the next +, and puts it in every panel's sheet.
func (d *dowt) setStart(s start) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.start == s {
		return
	}
	d.start = s
	if d.startFile != "" {
		b, _ := json.Marshal(s)
		os.MkdirAll(filepath.Dir(d.startFile), 0o700)
		if err := os.WriteFile(d.startFile, b, 0o600); err != nil {
			log.Print(err)
		}
	}
	for sess, names := range d.subs {
		for name := range names {
			sess.send(map[string]any{"version": "1.0", "updateDataModel": map[string]any{"surfaceId": name, "path": "/start", "value": s}})
		}
	}
}

type status struct {
	branch string
	review *review
	err    error
	at     time.Time // when last looked up; zero is never
	// failing is since when lookups have failed; zero while they don't.
	failing time.Time
	// good is the review the last lookup that didn't fail found, and goodAt
	// when; zero for none yet. The badge goes on showing it while lookups
	// fail, for up to unknownAfter, as ccwt's does.
	good   *review
	goodAt time.Time
	busy   bool
	note   string // what the last + or ✓ did
	made   int    // worktrees the + made here: a new one closes its sheet and empties its field

	// unsafe is why removing the worktree would lose something, "" when it
	// wouldn't; checked is when that was last asked, zero for never.
	unsafe   string
	checked  time.Time
	checking bool
}

func newDowt(dn *dennet, endpoint string) *dowt {
	return &dowt{dn: dn, endpoint: endpoint, sem: make(chan struct{}, lookups),
		spaces: map[string]space{}, status: map[string]*status{}, subs: map[*session]map[string]bool{}}
}

type session struct {
	mu   sync.Mutex
	conn net.Conn
}

func (s *session) send(msg any) {
	b, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) // a stuck frontend doesn't stall the rest
	s.conn.Write(append(b, '\n'))
}

// serve is one frontend's session. dennet connects too, to see that we're
// alive; it never says hello, so it gets nothing.
func (d *dowt) serve(s *session) {
	defer func() {
		d.mu.Lock()
		delete(d.subs, s)
		d.mu.Unlock()
		s.conn.Close()
	}()
	r := bufio.NewScanner(s.conn)
	r.Buffer(nil, 1<<20)
	for r.Scan() {
		var msg struct {
			Dense *struct {
				Hello *struct {
					Surfaces []string `json:"surfaces"`
				} `json:"hello"`
				Subscribe, Unsubscribe *struct {
					SurfaceID string `json:"surfaceId"`
				}
			} `json:"dense"`
			Action *struct {
				Name      string `json:"name"`
				SurfaceID string `json:"surfaceId"`
				Context   struct {
					Title   string `json:"title"`
					Command string `json:"command"`
					Run     bool   `json:"run"`
				} `json:"context"`
			} `json:"action"`
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(r.Bytes(), &msg); err != nil {
			log.Printf("bad line: %v", err)
			continue
		}
		switch {
		case msg.Dense != nil && msg.Dense.Hello != nil:
			for _, name := range msg.Dense.Hello.Surfaces {
				d.subscribe(s, name)
			}
		case msg.Dense != nil && msg.Dense.Subscribe != nil:
			d.subscribe(s, msg.Dense.Subscribe.SurfaceID)
		case msg.Dense != nil && msg.Dense.Unsubscribe != nil:
			d.mu.Lock()
			delete(d.subs[s], msg.Dense.Unsubscribe.SurfaceID)
			d.mu.Unlock()
		case msg.Action != nil && msg.Action.Name == "newWorktree":
			c := msg.Action.Context
			go d.newWorktree(msg.Action.SurfaceID, strings.TrimSpace(c.Title), start{strings.TrimSpace(c.Command), c.Run})
		case msg.Action != nil && msg.Action.Name == "removeWorktree":
			go d.removeWorktree(msg.Action.SurfaceID, false)
		case msg.Action != nil && msg.Action.Name == "forceRemoveWorktree":
			go d.removeWorktree(msg.Action.SurfaceID, true)
		case msg.Error != nil:
			log.Printf("dense reports an error: %v", msg.Error)
		}
	}
}

func (d *dowt) subscribe(s *session, name string) {
	d.mu.Lock()
	if d.subs[s] == nil {
		d.subs[s] = map[string]bool{}
	}
	d.subs[s][name] = true
	msg := d.create(name)
	d.mu.Unlock()
	s.send(msg)
	d.poll()
}

// create is the createSurface that draws a panel afresh. Call with d.mu held.
func (d *dowt) create(name string) map[string]any {
	return map[string]any{"version": "1.0", "createSurface": map[string]any{
		"surfaceId": name, "catalogId": denseCatalog, "components": d.view(name), "dataModel": map[string]any{"start": d.start},
	}}
}

// setSpaces is what reconcile last saw; a change redraws every panel.
func (d *dowt) setSpaces(spaces map[string]space) {
	d.mu.Lock()
	same := maps.Equal(d.spaces, spaces)
	d.spaces = spaces
	d.mu.Unlock()
	if !same {
		d.redraw("")
	}
	d.poll()
}

// dirOf is the working directory of the Space a panel is in. Call with d.mu held.
func (d *dowt) dirOf(surface string) string {
	return d.spaces[strings.TrimPrefix(surface, prefix)].Spec.WorkingDir
}

// redraw sends every panel on dir ("" for all) as it is now, to whoever shows it.
func (d *dowt) redraw(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for s, names := range d.subs {
		for name := range names {
			if dir == "" || d.dirOf(name) == dir {
				s.send(map[string]any{"version": "1.0", "updateComponents": map[string]any{
					"surfaceId": name, "components": d.view(name),
				}})
			}
		}
	}
}

func (d *dowt) pollLoop() {
	for range time.Tick(10 * time.Second) {
		d.poll()
	}
}

// fetchLoop keeps origin's main fresh in the repo of every worktree a Space
// is on, as a hand-run fetch would: the ✓ asks whether a branch is merged
// into it. One repo after another, and the wait after the round, so a slow
// fetch doesn't pile up on the next.
func (d *dowt) fetchLoop() {
	for {
		time.Sleep(fetchEvery)
		for _, root := range d.roots() {
			fetchMain(root)
		}
	}
}

// roots is the repos the live Spaces' worktrees are of, each once.
func (d *dowt) roots() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var roots []string
	for _, sp := range d.spaces {
		if root, wt := worktreeOf(sp.Spec.WorkingDir); wt != "" && !sp.Spec.Archived && !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots
}

// poll looks up again every shown panel's review that is older than
// refreshEvery, and asks again whether its worktree is safe to remove once
// that answer is older than recheckEvery.
//
// ponytail: dense subscribes every panel, shown or not, so this is every
// Space's review; ask dense for the shown ones if gh/glab start to complain.
func (d *dowt) poll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, names := range d.subs {
		for name := range names {
			dir := d.dirOf(name)
			if dir == "" {
				continue
			}
			st := d.status[dir]
			if st == nil {
				st = &status{}
				d.status[dir] = st
			}
			if !st.busy && time.Since(st.at) >= refreshEvery {
				st.busy = true
				go func() {
					branch := gitOut(dir, "branch", "--show-current")
					d.update(dir, func(st *status) { st.branch = branch })
					d.sem <- struct{}{}
					branch, r, err := lookup(dir)
					<-d.sem
					// Merged on the forge, not by the last check: origin/main
					// may not have it yet. Fetch it now, and ask again.
					d.mu.Lock()
					stale := d.status[dir].unsafe != ""
					d.mu.Unlock()
					root, wt := worktreeOf(dir)
					stale = stale && wt != "" && r != nil && r.State == "merged"
					if stale {
						fetchMain(root)
					}
					var show *review
					var showErr error
					var goodAt time.Time
					var link string
					d.update(dir, func(st *status) {
						st.branch, st.review, st.err, st.at, st.busy = branch, r, err, time.Now(), false
						if stale {
							st.checked = time.Time{}
						}
						if _, none := errors.AsType[noReview](err); err == nil || none {
							st.failing, st.good, st.goodAt = time.Time{}, r, st.at
						} else if st.failing.IsZero() {
							st.failing = st.at
						}
						// A failed lookup is no news: the badge goes on with the
						// last good one until the failures have lasted unknownAfter.
						show, goodAt = st.good, st.goodAt
						if st.good != nil {
							link = st.good.URL
						}
						if !st.failing.IsZero() && time.Since(st.failing) >= unknownAfter {
							show, showErr = nil, err
						}
					})
					if showErr != nil || !goodAt.IsZero() {
						dirty := show != nil && show.State == "merged" && gitOut(dir, "status", "--porcelain") != ""
						d.badge(dir, glyph(show, showErr, dirty), link)
					}
					if stale {
						d.poll()
					}
				}()
			}
			if root, wt := worktreeOf(dir); wt != "" && !st.checking && time.Since(st.checked) >= recheckEvery {
				st.checking = true
				go func() {
					d.sem <- struct{}{}
					err := d.unsafe(root, wt)
					<-d.sem
					d.update(dir, func(st *status) {
						st.unsafe, st.checked, st.checking = errText(err), time.Now(), false
					})
				}()
			}
		}
	}
}

// reviewLink is the annotation that links a Space to its worktree's PR or MR.
// Links to what a Space is about are annotations link.den.mkm.pub/<kind>, one
// link each, which any tool may write; pasting one into dense's Open Space (⌘K)
// or sidebar filter finds the Space. The kind is what the link is, not who
// writes it, so ccwt can write the same one and the two agree.
const reviewLink = "link.den.mkm.pub/review"

// unknownAfter is how long lookups have to go on failing before the badge
// says "?", as ccwt's does.
const unknownAfter = 10 * time.Minute

// ccwtBadges are the glyphs glyph puts on a Space, which are how badge tells
// its own from anyone else's: dennet's badges have no owner. They are ccwt's,
// so the two agree and neither leaves the other's behind.
var ccwtBadges = []string{"✓", "±", "☐", "■", "▣", "☒", "?"}

// glyph is ccwt's badge for where a review stands: "✓" merged ("±" if the
// worktree has uncommitted changes, so it doesn't read as "remove it"); while
// open, "☐" waiting on review, "■" on a merge train or queue, "▣" CI running,
// "☒" CI failed; "?" when the forge couldn't be asked; none for no review or
// a closed one.
func glyph(r *review, err error, dirty bool) string {
	switch {
	case err != nil:
		return "?"
	case r == nil || r.State == "closed":
		return ""
	case r.State == "merged" && dirty:
		return "±"
	case r.State == "merged":
		return "✓"
	case r.Queued:
		return "■"
	case r.CI == "running":
		return "▣"
	case r.CI == "failed":
		return "☒"
	}
	return "☐"
}

// badge swaps our badge on every Space on dir for g, or takes it off for "",
// and sets the Space's reviewLink to link, the review's URL, or takes it off
// for "". It writes nothing when that changes nothing. The resourceVersion goes with
// the write, so a badge someone else added meanwhile isn't written over: that
// write fails, and the next lookup tries again.
func (d *dowt) badge(dir, g, link string) {
	type badge struct {
		Text string `json:"text"`
		Tone string `json:"tone,omitempty"`
	}
	d.mu.Lock()
	var names []string
	for name, sp := range d.spaces {
		if sp.Spec.WorkingDir == dir && !sp.Spec.Archived {
			names = append(names, name)
		}
	}
	d.mu.Unlock()
	for _, name := range names {
		var sp struct {
			Metadata struct {
				ResourceVersion string            `json:"resourceVersion"`
				Annotations     map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				WorkingDir string  `json:"workingDir"`
				Archived   bool    `json:"archived"`
				Badges     []badge `json:"badges"`
			} `json:"spec"`
		}
		if err := d.dn.do(http.MethodGet, "spaces/"+name, nil, &sp); err != nil {
			log.Print(err)
			continue
		}
		if sp.Spec.WorkingDir != dir || sp.Spec.Archived { // moved on since
			continue
		}
		badges := slices.DeleteFunc(slices.Clone(sp.Spec.Badges), func(b badge) bool { return slices.Contains(ccwtBadges, b.Text) })
		if g != "" {
			badges = append(badges, badge{Text: g})
		}
		if slices.Equal(badges, sp.Spec.Badges) && sp.Metadata.Annotations[reviewLink] == link {
			continue
		}
		var linkValue any // null takes it off
		if link != "" {
			linkValue = link
		}
		err := d.dn.do(http.MethodPatch, "spaces/"+name, map[string]any{
			"metadata": map[string]any{
				"resourceVersion": sp.Metadata.ResourceVersion,
				"annotations":     map[string]any{reviewLink: linkValue},
			},
			"spec": map[string]any{"badges": badges},
		}, nil)
		if err != nil {
			log.Print(err)
		}
	}
}

// newWorktree is the Create of the +'s sheet: a new worktree beside the
// panel's, with a random name, and a Space on it titled title (the worktree's
// name if ""), which the window clicked in then shows. Its terminal runs
// s's command, if s says to run it.
func (d *dowt) newWorktree(surface, title string, s start) {
	d.setStart(s)
	cmd := ""
	if s.Run {
		cmd = s.Command
	}
	d.mu.Lock()
	sp, ok := d.spaces[strings.TrimPrefix(surface, prefix)]
	d.mu.Unlock()
	dir := sp.Spec.WorkingDir
	if !ok || dir == "" {
		return
	}
	d.note(dir, "creating a worktree…")
	path, err := makeWorktree(dir)
	if err == nil {
		var name string
		if name, err = d.newSpace(sp, path, title, cmd); err == nil {
			d.update(dir, func(st *status) { st.note, st.made = "made Space "+name, st.made+1 })
			d.show(surface, name)
			return
		}
	}
	d.note(dir, "+ failed: "+err.Error())
}

// removeWorktree is the ✓: it closes the Spaces on the worktree, with their
// tabs and terminals, and removes the worktree and its branch. Unless forced
// (Remove anyway, from the sheet the ✓ opens when it isn't safe), it refuses
// when that would lose something; one that was safe when last asked and isn't
// any more is refused, and its ✓ turns grey.
func (d *dowt) removeWorktree(surface string, force bool) {
	d.mu.Lock()
	dir := d.dirOf(surface)
	d.mu.Unlock()
	root, name := worktreeOf(dir)
	if name == "" {
		return
	}
	d.note(dir, "removing "+name+"…")
	var err error
	if !force {
		err = d.unsafe(root, name)
	}
	if err == nil {
		// Before the directory they're in goes away.
		err = d.dn.closeSpaces(dir, filepath.Join(realpath(root), ".claude", "worktrees", name))
	}
	if err == nil {
		err = deleteWorktree(root, name)
	}
	d.update(dir, func(st *status) {
		switch {
		case err == nil:
			st.note = "removed " + name
		case force:
			st.note = "removing failed: " + errText(err)
		default:
			st.note, st.unsafe, st.checked = "", errText(err), time.Now()
		}
	})
}

// unsafe is why removing the worktree name of the repo at root would lose
// something, nil when it wouldn't.
func (d *dowt) unsafe(root, name string) error {
	busy, err := d.dn.busyCwds()
	if err != nil {
		return err
	}
	return unsafeToRemove(root, name, busy)
}

// tool is a button of the panel's toolbar, drawn as dense draws its own: an
// SF Symbol, grey, or in the color of kind (a StatusBadge's: ok is green).
// Without an event it does nothing itself, and a Modal uses it as its trigger.
func tool(id, icon, kind, help, event string) map[string]any {
	c := map[string]any{"id": id, "component": "ToolbarButton", "icon": icon, "kind": kind, "help": help}
	if event != "" {
		c["action"] = map[string]any{"event": map[string]any{"name": event}}
	}
	return c
}

// newSheet is the +: a click opens a sheet asking the new Space's title, and
// what to run in its terminal. Its id and the title's data path change with
// every worktree made, so the sheet closes, and opens again with an empty
// title; the command and its checkbox stay as they were (see setStart).
func newSheet(st *status) (string, []any) {
	id, path := fmt.Sprintf("new%d", st.made), fmt.Sprintf("/name%d", st.made)
	return id, []any{
		map[string]any{"id": id, "component": "Modal", "trigger": "new_mark", "content": "new_sheet"},
		tool("new_mark", "plus", "", "New worktree, beside this one", ""),
		map[string]any{"id": "new_sheet", "component": "Column", "children": []string{"new_title", "new_name", "new_run", "new_command", "new_note", "create"}},
		map[string]any{"id": "new_title", "component": "Text", "text": "New worktree"},
		map[string]any{"id": "new_name", "component": "TextField", "label": "Title", "placeholder": "empty for the worktree's name",
			"value": map[string]any{"@path": path}},
		map[string]any{"id": "new_run", "component": "CheckBox", "label": "Run in its terminal", "value": map[string]any{"@path": "/start/run"}},
		map[string]any{"id": "new_command", "component": "TextField", "label": "Command", "placeholder": "claude, codex, …",
			"value": map[string]any{"@path": "/start/command"}},
		map[string]any{"id": "new_note", "component": "Text", "text": st.note, "variant": "caption"},
		map[string]any{"id": "create", "component": "Button", "variant": "primary", "child": "create_text",
			"action": map[string]any{"event": map[string]any{"name": "newWorktree", "context": map[string]any{
				"title": map[string]any{"@path": path}, "command": map[string]any{"@path": "/start/command"}, "run": map[string]any{"@path": "/start/run"}}}}},
		map[string]any{"id": "create_text", "component": "Text", "text": "Create"},
	}
}

// done is the ✓ that cleans the worktree up. Green when removing it is safe,
// and a click removes it. Grey until that's known, and a click asks
// the removal, which refuses if it isn't. Grey when it isn't safe, and a click
// opens a sheet saying why, with Remove anyway; its Close is the way out.
func (d *dowt) done(st *status) []any {
	const help = "Done: remove this worktree, its branch and its Space"
	switch {
	case st.checked.IsZero():
		return []any{tool("done", "checkmark", "", help, "removeWorktree")}
	case st.unsafe == "":
		return []any{tool("done", "checkmark", "ok", help, "removeWorktree")}
	}
	return []any{
		map[string]any{"id": "done", "component": "Modal", "trigger": "done_mark", "content": "sheet"},
		tool("done_mark", "checkmark", "", "Not safe to remove: "+st.unsafe, ""),
		map[string]any{"id": "sheet", "component": "Column", "children": []string{"sheet_title", "sheet_why", "sheet_note", "force"}},
		map[string]any{"id": "sheet_title", "component": "Text", "text": "Not safe to remove this worktree"},
		map[string]any{"id": "sheet_why", "component": "Text", "text": st.unsafe, "variant": "caption"},
		map[string]any{"id": "sheet_note", "component": "Text", "text": st.note, "variant": "caption"},
		map[string]any{"id": "force", "component": "Button", "variant": "primary", "child": "force_text",
			"action": map[string]any{"event": map[string]any{"name": "forceRemoveWorktree"}}},
		map[string]any{"id": "force_text", "component": "Text", "text": "Remove anyway"},
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// worktreeOf is the repo and the name of the worktree at dir,
// <root>/.claude/worktrees/<name>, or "" for any other directory.
func worktreeOf(dir string) (root, name string) {
	parent := filepath.Dir(dir)
	if dir == "" || filepath.Base(parent) != "worktrees" || filepath.Base(filepath.Dir(parent)) != ".claude" {
		return "", ""
	}
	return filepath.Dir(filepath.Dir(parent)), filepath.Base(dir)
}

func (d *dowt) note(dir, text string) {
	d.update(dir, func(st *status) { st.note = text })
}

// update changes dir's status and redraws its panels.
func (d *dowt) update(dir string, change func(*status)) {
	d.mu.Lock()
	if d.status[dir] == nil {
		d.status[dir] = &status{}
	}
	change(d.status[dir])
	d.mu.Unlock()
	d.redraw(dir)
}

// view is a panel's components, all of them, every time: updateComponents
// replaces by id, and what root no longer lists isn't drawn. Call with d.mu held.
func (d *dowt) view(surface string) []any {
	text := func(id, s string) map[string]any {
		return map[string]any{"id": id, "component": "Text", "text": s, "variant": "caption"}
	}
	badge := func(id, kind, s string) map[string]any {
		return map[string]any{"id": id, "component": "StatusBadge", "kind": kind, "text": s}
	}
	dir := d.dirOf(surface)
	st := d.status[dir]
	if st == nil {
		st = &status{}
	}

	newID, newSheet := newSheet(st)
	buttons := []string{}
	if dir != "" {
		buttons = append(buttons, newID)
	}
	if _, wt := worktreeOf(dir); wt != "" {
		buttons = append(buttons, "done")
	}
	children := []string{"head"}
	c := []any{
		map[string]any{"id": "head", "component": "Row", "justify": "spaceBetween", "align": "center", "children": []string{"branch", "buttons"}},
		map[string]any{"id": "buttons", "component": "Row", "align": "center", "children": buttons},
		text("branch", st.branch),
	}
	c = append(c, newSheet...)
	c = append(c, d.done(st)...)

	r := st.review
	switch {
	case dir == "":
		c, children = append(c, text("msg", "no working directory")), append(children, "msg")
	case st.at.IsZero():
		c, children = append(c, text("msg", "looking for a review…")), append(children, "msg")
	case st.err != nil:
		c, children = append(c, text("msg", "? "+st.err.Error())), append(children, "msg")
	case r == nil:
		c, children = append(c, text("msg", "no PR/MR yet")), append(children, "msg")
	default:
		badges := []string{"state"}
		// Color only what asks for attention: red is broken, yellow is my
		// move, green is done. The normal path (open, CI passed) stays grey.
		c = append(c, badge("state", map[string]string{"merged": "ok"}[r.State], r.State))
		if r.Approval != "" {
			badges = append(badges, "approval")
			kind, label := "", "needs approval"
			switch r.Approval {
			case "approved":
				label = "approved"
				if r.State == "open" { // merging is on me now
					kind = "warn"
				}
			case "changes":
				kind, label = "warn", "changes requested"
			}
			c = append(c, badge("approval", kind, label))
		}
		if r.CI != "" {
			badges = append(badges, "ci")
			kind := map[string]string{"failed": "error", "manual": "warn"}[r.CI]
			c = append(c, badge("ci", kind, "CI "+r.CI))
		}
		c = append(c,
			map[string]any{"id": "badges", "component": "Row", "children": badges},
			text("link_text", r.Ref+" "+r.Title),
		)
		children = append(children, "badges", "link_text")
		if r.URL != "" { // dense refuses an openUrl of ""
			c = append(c, map[string]any{"id": "link", "component": "Button", "variant": "borderless", "child": "link_text",
				"action": map[string]any{"functionCall": map[string]any{"@call": "openUrl", "args": map[string]any{"url": r.URL}}}})
			children[len(children)-1] = "link"
		}
	}
	if st.note != "" {
		c, children = append(c, text("note", st.note)), append(children, "note")
	}
	return append(c, map[string]any{"id": "root", "component": "Column", "children": children})
}
