package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"log"
	"maps"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	denseCatalog = "https://dense.mkm.pub/a2ui/v1/catalog.json"
	// refreshEvery is how old a review may get before it is asked for again.
	refreshEvery = 2 * time.Minute
	// lookups is how many gh/glab lookups may run at once.
	lookups = 4
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
}

type status struct {
	branch string
	review *review
	err    error
	at     time.Time // when last looked up; zero is never
	busy   bool
	note   string // what the last + did
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
			go d.newWorktree(msg.Action.SurfaceID)
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
	components := d.view(name)
	d.mu.Unlock()
	s.send(map[string]any{"version": "1.0", "createSurface": map[string]any{
		"surfaceId": name, "catalogId": denseCatalog, "components": components, "dataModel": map[string]any{},
	}})
	d.poll()
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

// poll looks up again every shown panel's review that is older than refreshEvery.
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
			if st.busy || time.Since(st.at) < refreshEvery {
				continue
			}
			st.busy = true
			go func() {
				d.sem <- struct{}{}
				branch, r, err := lookup(dir)
				<-d.sem
				d.mu.Lock()
				st.branch, st.review, st.err, st.at, st.busy = branch, r, err, time.Now(), false
				d.mu.Unlock()
				d.redraw(dir)
			}()
		}
	}
}

// newWorktree is the +: `ccwt new` beside the panel's worktree, and a Space on
// the new one. Starting an agent in it is left to you.
func (d *dowt) newWorktree(surface string) {
	d.mu.Lock()
	sp, ok := d.spaces[strings.TrimPrefix(surface, prefix)]
	d.mu.Unlock()
	dir := sp.Spec.WorkingDir
	if !ok || dir == "" {
		return
	}
	d.note(dir, "creating a worktree…")
	out, err := run(dir, "ccwt", "new", "--force-create", "--path")
	path := strings.TrimSpace(string(out))
	if err == nil {
		var name string
		if name, err = d.newSpace(sp, path); err == nil {
			d.note(dir, "made Space "+name)
			return
		}
	}
	d.note(dir, "+ failed: "+err.Error())
}

func (d *dowt) note(dir, text string) {
	d.mu.Lock()
	if d.status[dir] == nil {
		d.status[dir] = &status{}
	}
	d.status[dir].note = text
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

	head := []string{"branch"}
	if dir != "" {
		head = append(head, "new")
	}
	children := []string{"head"}
	c := []any{
		map[string]any{"id": "head", "component": "Row", "justify": "spaceBetween", "align": "center", "children": head},
		text("branch", cmp.Or(st.branch, "—")),
		map[string]any{"id": "new", "component": "Button", "variant": "borderless", "child": "new_icon",
			"action": map[string]any{"event": map[string]any{"name": "newWorktree"}}},
		map[string]any{"id": "new_icon", "component": "Icon", "name": "add"},
	}

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
		c = append(c, badge("state", map[string]string{"open": "busy", "merged": "ok"}[r.State], r.State))
		if r.Approval != "" {
			badges = append(badges, "approval")
			kind, label := "warn", "needs approval"
			switch r.Approval {
			case "approved":
				kind, label = "ok", "approved"
			case "changes":
				kind, label = "error", "changes requested"
			}
			c = append(c, badge("approval", kind, label))
		}
		if r.CI != "" {
			badges = append(badges, "ci")
			kind := map[string]string{"running": "busy", "failed": "error", "passed": "ok", "manual": "warn"}[r.CI]
			c = append(c, badge("ci", kind, "CI "+r.CI))
		}
		c = append(c,
			map[string]any{"id": "badges", "component": "Row", "children": badges},
			map[string]any{"id": "link", "component": "Button", "variant": "borderless", "child": "link_text",
				"action": map[string]any{"functionCall": map[string]any{"@call": "openUrl", "args": map[string]any{"url": r.URL}}}},
			text("link_text", r.Ref+" "+r.Title),
		)
		children = append(children, "badges", "link")
	}
	if st.note != "" {
		c, children = append(c, text("note", st.note)), append(children, "note")
	}
	return append(c, map[string]any{"id": "root", "component": "Column", "children": children})
}
