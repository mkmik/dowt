// Command dowt is a dense Extension: it puts a panel in the "details" box of
// every Space, saying where the review (PR or MR) of the Space's worktree
// stands, with a + that makes a new worktree and a Space for it.
//
// It is a controller: it watches dennet's Spaces and Surfaces and keeps one
// ui Surface of its own per Space, named "dowt-<space>", creating the missing
// ones and deleting the ones whose Space is gone. dense connects to dowt's
// socket and asks for what is in them (see ui.go).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	extName = "dowt"
	box     = "details"
	prefix  = extName + "-"
)

func main() {
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".local", "state")
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		state = d
	}
	sock := flag.String("socket", filepath.Join(state, "dennet", "dennet.sock"), "dennet's socket")
	ns := flag.String("namespace", "default", "namespace whose Spaces get a panel")
	endpoint := flag.String("endpoint", filepath.Join(state, "dense", "ext", extName+".sock"), "socket dowt listens on, for dense")
	flag.Parse()

	os.MkdirAll(filepath.Dir(*endpoint), 0o700)
	os.Remove(*endpoint)
	ln, err := net.Listen("unix", *endpoint)
	if err != nil {
		log.Fatal(err)
	}
	os.Chmod(*endpoint, 0o600)

	d := newDowt(&dennet{
		api: "http://dennet/apis/den.mkm.pub/v1alpha1/namespaces/" + *ns + "/",
		c: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *sock)
		}}},
	}, *endpoint)

	kick := make(chan struct{}, 1)
	for _, res := range []string{"spaces", "surfaces"} {
		go d.dn.watch(res, kick)
	}
	go d.reconcileLoop(kick)
	go d.pollLoop()
	log.Printf("listening on %s", *endpoint)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go d.serve(&session{conn: conn})
	}
}

// dennet is a client of dennet's API, in one namespace.
type dennet struct {
	api string
	c   *http.Client
}

// do sends body as JSON (a merge patch for PATCH) and decodes the answer into
// out. A status of 300 or more is an error that carries the status code.
func (dn *dennet) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, dn.api+path, r)
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	resp, err := dn.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &apiError{resp.StatusCode, fmt.Sprintf("%s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(b))}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func isCode(err error, code int) bool {
	e, ok := err.(*apiError)
	return ok && e.code == code
}

// watch pokes kick on every change to res, for good. A watch from no
// resourceVersion starts with every object, so a dropped watch loses nothing:
// the reconcile after it looks at everything anyway.
func (dn *dennet) watch(res string, kick chan<- struct{}) {
	for {
		resp, err := dn.c.Get(dn.api + res + "?watch=1")
		if err == nil {
			for dec := json.NewDecoder(resp.Body); dec.Decode(new(json.RawMessage)) == nil; {
				poke(kick)
			}
			resp.Body.Close()
		}
		time.Sleep(time.Second)
	}
}

func poke(c chan<- struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

type objectMeta struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type space struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Parent     string `json:"parent"`
		Order      int32  `json:"order"`
		Title      string `json:"title"`
		WorkingDir string `json:"workingDir"`
		Archived   bool   `json:"archived"`
	} `json:"spec"`
}

type surface struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Space string `json:"space"`
		UI    *struct {
			Extension string `json:"extension"`
		} `json:"ui"`
	} `json:"spec"`
}

func list[T any](dn *dennet, res string) ([]T, error) {
	var l struct{ Items []T }
	err := dn.do(http.MethodGet, res, nil, &l)
	return l.Items, err
}

func (d *dowt) reconcileLoop(kick <-chan struct{}) {
	for {
		select {
		case <-kick:
		case <-time.After(30 * time.Second):
		}
		time.Sleep(200 * time.Millisecond) // a burst of events is one reconcile
		select {
		case <-kick:
		default:
		}
		if err := d.reconcile(); err != nil {
			log.Print(err)
		}
	}
}

// reconcile makes dennet hold one Surface of ours for each live Space and no
// others. A panel closed in dense comes back: the Space should have one.
func (d *dowt) reconcile() error {
	uid, err := d.applyExtension()
	if err != nil {
		return err
	}
	spaces, err := list[space](d.dn, "spaces")
	if err != nil {
		return err
	}
	surfaces, err := list[surface](d.dn, "surfaces")
	if err != nil {
		return err
	}
	byName := map[string]space{}
	for _, s := range spaces {
		byName[s.Metadata.Name] = s
	}
	d.setSpaces(byName)

	have := map[string]bool{}
	for _, s := range surfaces {
		if s.Spec.UI == nil || s.Spec.UI.Extension != extName {
			continue
		}
		have[s.Metadata.Name] = true
		if sp, ok := byName[s.Spec.Space]; !ok || sp.Spec.Archived || s.Metadata.Name != prefix+s.Spec.Space {
			if err := d.dn.do(http.MethodDelete, "surfaces/"+s.Metadata.Name, nil, nil); err != nil && !isCode(err, http.StatusNotFound) {
				log.Print(err)
			}
		}
	}
	owner := []any{map[string]any{"apiVersion": "den.mkm.pub/v1alpha1", "kind": "Extension", "name": extName, "uid": uid}}
	for _, sp := range spaces {
		name := prefix + sp.Metadata.Name
		if sp.Spec.Archived || have[name] {
			continue
		}
		err := d.dn.do(http.MethodPost, "surfaces", map[string]any{
			"metadata": map[string]any{"name": name, "ownerReferences": owner},
			"spec": map[string]any{"space": sp.Metadata.Name, "title": "Review", "placement": box,
				"kind": "ui", "ui": map[string]any{"extension": extName}},
		}, nil)
		if err != nil && !isCode(err, http.StatusConflict) {
			log.Print(err)
		}
	}
	return nil
}

// applyExtension makes sure Extension/dowt is there and names our socket, and
// returns its uid, for the Surfaces it owns: dennet deletes them some time
// after dowt stops for good.
func (d *dowt) applyExtension() (string, error) {
	var ext struct{ Metadata objectMeta }
	spec := map[string]any{"endpoint": d.endpoint}
	err := d.dn.do(http.MethodPatch, "extensions/"+extName, map[string]any{"spec": spec}, &ext)
	if isCode(err, http.StatusNotFound) {
		err = d.dn.do(http.MethodPost, "extensions", map[string]any{
			"apiVersion": "den.mkm.pub/v1alpha1", "kind": "Extension",
			"metadata": map[string]any{"name": extName}, "spec": spec,
		}, &ext)
	}
	return ext.Metadata.UID, err
}

// newSpace makes a Space on the worktree at path, last among the repo's
// worktrees: under the Space on the repo's main checkout, else beside the
// Space the + was clicked in. Named after the directory, as ccwt names them.
func (d *dowt) newSpace(from space, path string) (string, error) {
	root := strings.TrimSuffix(gitOut(path, "rev-parse", "--path-format=absolute", "--git-common-dir"), "/.git")
	spaces, err := list[space](d.dn, "spaces")
	if err != nil {
		return "", err
	}
	parent := from.Spec.Parent
	for _, s := range spaces {
		if s.Spec.WorkingDir == root && !s.Spec.Archived {
			parent = s.Metadata.Name
			break
		}
	}
	order := int32(0)
	for _, s := range spaces {
		if s.Spec.Parent == parent {
			order = max(order, s.Spec.Order+1)
		}
	}
	spec := map[string]any{"title": filepath.Base(path), "workingDir": path, "order": order, "parent": parent}
	var made space
	meta := map[string]any{"name": dnsLabel(filepath.Base(path))}
	if meta["name"] == "" {
		meta = map[string]any{"generateName": "space-"}
	}
	err = d.dn.do(http.MethodPost, "spaces", map[string]any{"metadata": meta, "spec": spec}, &made)
	if isCode(err, http.StatusConflict) {
		err = d.dn.do(http.MethodPost, "spaces", map[string]any{"metadata": map[string]any{"generateName": "space-"}, "spec": spec}, &made)
	}
	if err != nil {
		return "", err
	}
	return made.Metadata.Name, d.newTerminal(made.Metadata.Name, path)
}

// newTerminal makes a terminal tab in space, in dir: a Den, which dennet
// starts, and a Surface showing it, named after the Den, as dense and ccwt
// make a new tab.
func (d *dowt) newTerminal(space, dir string) error {
	var den struct{ Metadata objectMeta }
	if err := d.dn.do(http.MethodPost, "dens", map[string]any{
		"metadata": map[string]any{"generateName": space + "-"},
		"spec":     map[string]any{"workingDir": dir, "inheritEnv": []string{"LANG", "LC_*", "SSH_AUTH_SOCK", "TMPDIR"}},
	}, &den); err != nil {
		return err
	}
	err := d.dn.do(http.MethodPost, "surfaces", map[string]any{
		"metadata": map[string]any{"name": den.Metadata.Name},
		"spec": map[string]any{"space": space, "order": 1, "kind": "terminal",
			"terminal": map[string]any{"den": den.Metadata.Name}},
	}, nil)
	if err != nil {
		d.dn.do(http.MethodDelete, "dens/"+den.Metadata.Name, nil, nil) // a den no tab shows is one nobody would find
	}
	return err
}

// dnsLabel is name as a Kubernetes name can have it, or "" when nothing is left.
func dnsLabel(name string) string {
	b := []byte(strings.ToLower(name))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-")
}
