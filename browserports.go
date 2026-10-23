package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// What runs on localhost, and running a project: the Browser's ports list (every TCP server listening on this box,
// named by its process, its project folder and the page's <title>), the projects it can start (a package.json's dev
// script in a zellij tab, a folder of HTML served by the panel itself), and stopping either.

type bport struct {
	Port    int    `json:"port"`
	Addr    string `json:"addr"`
	PID     int    `json:"pid,omitempty"`
	Proc    string `json:"proc,omitempty"`
	Cmd     string `json:"cmd,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Project string `json:"project,omitempty"`
	Pane    *bpane `json:"pane,omitempty"`
	Title   string `json:"title,omitempty"`
	Status  int    `json:"status,omitempty"` // 0: not HTTP (or no answer)
	Page    bool   `json:"page,omitempty"`   // its front page is a web page (not an API, a bare string, an error)
	Static  bool   `json:"static,omitempty"`
	Since   int64  `json:"since,omitempty"`
}

type bpane struct {
	Session string `json:"session"`
	Pane    int    `json:"pane"`
	Tab     string `json:"tab,omitempty"`
}

type bstatic struct {
	dir  string
	port int
	srv  *http.Server
}

type bprobe struct {
	at     time.Time
	title  string
	status int
	page   bool
}

var (
	brProbeMu sync.Mutex
	brProbes  = map[string]bprobe{} // "port/pid": what the page answered, a few seconds
)

// infrastructure that listens but isn't something to open: the panel itself, its tunnel, SSH, the agents' proxy
var brSkipComm = map[string]bool{"cloudflared": true, "sshd": true, "chrome": true, "chrome_crashpad": true, "headless_shell": true, "containerd": true, "dockerd": true}

// ports: the servers here that serve a web page; a backend (an API, a database, a proxy) isn't something to browse
func (b *browser) ports(w http.ResponseWriter, r *http.Request) {
	out := []bport{}
	for _, p := range b.listPorts() {
		if p.Page {
			out = append(out, p)
		}
	}
	writeJSON(w, 200, map[string]any{"ports": out})
}

func (b *browser) listPorts() []bport {
	sock := scanSockets("/proc")
	me := os.Getpid()
	b.mu.Lock()
	static := map[int]*bstatic{}
	for _, st := range b.static {
		static[st.port] = st
	}
	b.mu.Unlock()
	panelPorts := map[int]bool{}
	if b.s != nil {
		for _, a := range []string{b.s.cfg.addr, b.s.cfg.agentAddr} {
			if _, p, err := net.SplitHostPort(a); err == nil {
				n, _ := strconv.Atoi(p)
				panelPorts[n] = true
			}
		}
	}
	tabs := map[string]string{}
	if b.s != nil && b.s.term != nil {
		for _, s := range b.s.term.listing().Sessions {
			for _, tb := range s.Tabs {
				for _, p := range tb.Panes {
					tabs[s.Name+"."+strconv.Itoa(p.ID)] = tb.Name
				}
			}
		}
	}
	seen := map[int]bool{}
	var out []bport
	for _, e := range sock.entries {
		if e.Proto[:3] != "tcp" || e.State != "0A" {
			continue
		}
		port := int(e.Local.Port())
		a := e.Local.Addr()
		if seen[port] || !(a.IsLoopback() || a.IsUnspecified()) {
			continue
		}
		pid := sock.inodePID[e.Inode]
		p := bport{Port: port, Addr: a.String(), PID: pid}
		if st := static[port]; st != nil {
			p.Static, p.Dir, p.Project, p.Proc = true, tilde(st.dir), filepath.Base(st.dir), "panel"
		} else if pid == me || panelPorts[port] {
			continue
		} else if pid > 0 {
			p.Proc = procComm(pid)
			if brSkipComm[p.Proc] {
				continue
			}
			p.Cmd = trunc(procCmdline(pid), 300)
			cwd, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
			p.Dir, p.Project = tilde(cwd), projectOf(cwd)
			if fi, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
				p.Since = fi.ModTime().Unix()
			}
			if zs, zp := zellijOf(pid); zs != "" {
				p.Pane = &bpane{Session: zs, Pane: zp, Tab: tabs[zs+"."+strconv.Itoa(zp)]}
			}
		} else if port == 3456 { // the agents' API proxy, someone else's process
			continue
		}
		seen[port] = true
		out = append(out, p)
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].Title, out[i].Status, out[i].Page = probeHTTP(out[i].Port, out[i].PID)
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		hi, hj := out[i].Status > 0, out[j].Status > 0 // web servers first, then by port
		if hi != hj {
			return hi
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// zellijOf: the zellij session and pane a process (or the nearest of its parents) was started in.
func zellijOf(pid int) (string, int) {
	for i := 0; i < 8 && pid > 1; i++ {
		env, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		var zs string
		zp := -1
		for _, kv := range bytes.Split(env, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte("ZELLIJ_SESSION_NAME=")); ok {
				zs = string(v)
			} else if v, ok := bytes.CutPrefix(kv, []byte("ZELLIJ_PANE_ID=")); ok {
				zp, _ = strconv.Atoi(string(v))
			}
		}
		if zs != "" && zp >= 0 {
			return zs, zp
		}
		st, ok := procStartOf(pid)
		if !ok {
			break
		}
		pid = st.PPID
	}
	return "", -1
}

// projectOf: the repository a folder is in (its top folder's name), else the folder's own name.
func projectOf(dir string) string {
	if dir == "" {
		return ""
	}
	for d := dir; d != "/" && d != "." && d != homeDir; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return filepath.Base(d)
		}
	}
	return filepath.Base(dir)
}

var brTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// probeHTTP asks the server for its front page: what it's called, and whether it's a web page (a 2xx HTML document,
// after at most 3 redirects within localhost: a login page counts) rather than an API's JSON, a bare string or an
// error page. Kept 15 s.
func probeHTTP(port, pid int) (title string, status int, page bool) {
	k := strconv.Itoa(port) + "/" + strconv.Itoa(pid)
	brProbeMu.Lock()
	if p, ok := brProbes[k]; ok && time.Since(p.at) < 15*time.Second {
		brProbeMu.Unlock()
		return p.title, p.status, p.page
	}
	brProbeMu.Unlock()
	c := http.Client{Timeout: 800 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if h := r.URL.Hostname(); len(via) > 3 || !(h == "127.0.0.1" || h == "localhost" || h == "::1") {
				return http.ErrUseLastResponse
			}
			return nil
		}}
	if resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/"); err == nil {
		status = resp.StatusCode
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if m := brTitleRe.FindSubmatch(body); m != nil {
			title = trunc(strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " "), 120)
		}
		page = status/100 == 2 && strings.Contains(resp.Header.Get("Content-Type"), "text/html") && brDocRe.Match(body)
	}
	brProbeMu.Lock()
	brProbes[k] = bprobe{at: time.Now(), title: title, status: status, page: page}
	if len(brProbes) > 500 {
		clear(brProbes)
	}
	brProbeMu.Unlock()
	return title, status, page
}

// brDocRe: the start of an HTML document, not a string a framework sent as text/html
var brDocRe = regexp.MustCompile(`(?i)<(!doctype\s+html|html|head|body)[\s>]`)

// ---- projects ----

type bproject struct {
	Dir   string `json:"dir"`
	Name  string `json:"name"`
	Kind  string `json:"kind"` // node, static, python
	Cmd   string `json:"cmd"`
	Sub   string `json:"sub,omitempty"` // the folder the command runs in, when it isn't the project's top
	Mtime int64  `json:"mtime"`
	Ports []int  `json:"ports,omitempty"`
}

func (b *browser) projects(w http.ResponseWriter, r *http.Request) {
	var out []bproject
	seen := map[string]bool{}
	for _, root := range []string{"projects", "repos", "side-quests"} {
		ents, _ := os.ReadDir(filepath.Join(b.home, root))
		for _, e := range ents {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := filepath.Join(b.home, root, e.Name())
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if p, ok := detectProject(dir); ok {
				out = append(out, p)
			}
		}
	}
	running := map[string][]int{}
	for _, p := range b.listPorts() {
		if p.Dir != "" && p.Page {
			running[p.Dir] = append(running[p.Dir], p.Port)
		}
	}
	for i := range out {
		for d, ports := range running {
			if d == out[i].Dir || strings.HasPrefix(d, out[i].Dir+"/") {
				out[i].Ports = append(out[i].Ports, ports...)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	writeJSON(w, 200, map[string]any{"projects": out})
}

// detectProject: how a folder would be started. A package.json's dev (else start, serve, preview) script with the
// package manager its lockfile names, in the folder or a usual app subfolder; a Django manage.py; a plain index.html.
func detectProject(dir string) (bproject, bool) {
	p := bproject{Dir: tilde(dir), Name: filepath.Base(dir)}
	if fi, err := os.Stat(dir); err == nil {
		p.Mtime = fi.ModTime().Unix()
	}
	for _, sub := range []string{"", "web", "frontend", "app", "client", "site", "ui"} {
		d := filepath.Join(dir, sub)
		raw, err := os.ReadFile(filepath.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct{ Scripts map[string]string }
		if json.Unmarshal(raw, &pkg) != nil {
			continue
		}
		for _, s := range []string{"dev", "start", "serve", "preview"} {
			if pkg.Scripts[s] == "" {
				continue
			}
			pm := "npm"
			for lock, name := range map[string]string{"pnpm-lock.yaml": "pnpm", "yarn.lock": "yarn", "bun.lockb": "bun", "bun.lock": "bun"} {
				if _, err := os.Stat(filepath.Join(d, lock)); err == nil {
					pm = name
				}
			}
			run := pm + " run " + s
			if pm == "yarn" || pm == "pnpm" {
				run = pm + " " + s
			}
			if _, err := os.Stat(filepath.Join(d, "node_modules")); err != nil {
				run = pm + " install && " + run
			}
			p.Kind, p.Cmd, p.Sub = "node", run, sub
			return p, true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "manage.py")); err == nil {
		p.Kind, p.Cmd = "python", "python3 manage.py runserver"
		return p, true
	}
	for _, sub := range []string{"", "public", "dist", "site", "docs"} {
		if _, err := os.Stat(filepath.Join(dir, sub, "index.html")); err == nil {
			p.Kind, p.Cmd, p.Sub = "static", "", sub
			return p, true
		}
	}
	return p, false
}

// run starts a project: a static folder is served by the panel on a free port; anything else runs in a tab of
// the "servers" zellij session (made when missing), where its output stays readable from Terminals.
func (b *browser) run(w http.ResponseWriter, r *http.Request) {
	var body struct{ Dir, Cmd, Sub string }
	if readJSON(r, &body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	dir, ok := resolveDir(filepath.Join(body.Dir, body.Sub))
	if !ok || !(dir == b.home || strings.HasPrefix(dir, b.home+"/") || strings.HasPrefix(dir, "/tmp/")) {
		writeJSON(w, 400, map[string]any{"error": "that folder isn't under your home"})
		return
	}
	if strings.TrimSpace(body.Cmd) == "" { // static
		port, err := b.serveStatic(dir)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		b.s.audit.add("browser-serve", tilde(dir)+" on :"+strconv.Itoa(port), clientIP(r))
		writeJSON(w, 200, map[string]any{"ok": true, "port": port, "static": true})
		return
	}
	cmd := strings.TrimSpace(body.Cmd)
	if len(cmd) > 500 || strings.ContainsAny(cmd, "\n\r") {
		writeJSON(w, 400, map[string]any{"error": "one line, up to 500 characters"})
		return
	}
	const session = "servers"
	name := filepath.Base(filepath.Join(body.Dir))
	if n, ok := cleanName(name); ok {
		name = n
	}
	var pane int
	var err error
	if !b.s.sessionLive(session) {
		if err = b.s.z.newSession(session, dir); err == nil {
			err = fmt.Errorf("the servers session didn't start")
			for i := 0; i < 50; i++ {
				if ps, e := b.s.z.panes(session); e == nil && len(ps) > 0 {
					pane, err = ps[0].ID, nil
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err == nil { // the session's first tab, named for the project (a detached session has no focused tab)
				b.s.z.action(session, "rename-tab", "--tab-id", "0", name)
			}
		}
	} else {
		pane, err = b.s.z.newTab(session, name, dir)
	}
	if err == nil {
		time.Sleep(300 * time.Millisecond) // the shell's prompt first
		if err = b.s.z.write(session, pane, cmd); err == nil {
			err = b.s.z.keys(session, pane, "Enter")
		}
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	b.s.audit.add("browser-run", fmt.Sprintf("%s in %s (servers:%d)", trunc(cmd, 100), tilde(dir), pane), clientIP(r))
	select {
	case b.s.wake <- struct{}{}:
	default:
	}
	writeJSON(w, 200, map[string]any{"ok": true, "session": session, "pane": pane, "dir": tilde(dir)})
}

func (b *browser) serveStatic(dir string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.static[dir]; st != nil {
		return st.port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fsrv := http.FileServer(http.Dir(dir))
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// only as localhost (a page elsewhere can't reach it by a rebound name), and never a dotfile (.env, .git)
		if h, _, _ := net.SplitHostPort(r.Host); h != "localhost" && h != "127.0.0.1" {
			http.Error(w, "localhost only", http.StatusForbidden)
			return
		}
		for _, part := range strings.Split(r.URL.Path, "/") {
			if strings.HasPrefix(part, ".") && part != "." {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache") // edits show on reload
		fsrv.ServeHTTP(w, r)
	})}
	b.static[dir] = &bstatic{dir: dir, port: port, srv: srv}
	go srv.Serve(ln)
	return port, nil
}

// stop ends a server from the ports list: one the panel serves, or Ctrl-C in the pane a dev server runs in.
func (b *browser) stop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port int
	}
	if readJSON(r, &body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	b.mu.Lock()
	for d, st := range b.static {
		if st.port == body.Port {
			st.srv.Close()
			delete(b.static, d)
			b.mu.Unlock()
			b.s.audit.add("browser-stop", "static "+tilde(d), clientIP(r))
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	b.mu.Unlock()
	ps := b.listPorts()
	i := slices.IndexFunc(ps, func(p bport) bool { return p.Port == body.Port })
	if i < 0 || ps[i].Pane == nil || ps[i].PID <= 0 {
		writeJSON(w, 400, map[string]any{"error": "it isn't running in a terminal the panel can reach: stop it there"})
		return
	}
	p := ps[i]
	// Ctrl-C in the pane only when the server is what that terminal runs in the foreground: one an agent started in
	// the background lives in the agent's pane, and Ctrl-C there would interrupt the agent instead. Else a signal.
	how := "Ctrl-C in " + p.Pane.Session + ":" + strconv.Itoa(p.Pane.Pane)
	if st, ok := procStartOf(p.PID); ok && st.PGRP > 0 && st.PGRP == st.TPGID {
		if err := b.s.z.keys(p.Pane.Session, p.Pane.Pane, "Ctrl c"); err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
	} else if err := syscall.Kill(p.PID, syscall.SIGINT); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	} else {
		how = "SIGINT to " + strconv.Itoa(p.PID)
	}
	b.s.audit.add("browser-stop", fmt.Sprintf(":%d (%s)", p.Port, how), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// favicon: a site's icon for its tab, fetched here (the page's CSP keeps images to the panel). Kept an hour.
var (
	brIconTransport = &http.Transport{Proxy: nil, DisableKeepAlives: true}
	brIconMu        sync.Mutex
	brIcons         = map[string]struct {
		at   time.Time
		ct   string
		body []byte
	}{}
)

func (b *browser) favicon(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") || len(u) > 2000 {
		http.NotFound(w, r)
		return
	}
	brIconMu.Lock()
	ic, ok := brIcons[u]
	brIconMu.Unlock()
	if !ok || time.Since(ic.at) > time.Hour {
		c := http.Client{Timeout: 4 * time.Second, Transport: brIconTransport}
		ic.at = time.Now()
		ic.body, ic.ct = nil, ""
		if resp, err := c.Get(u); err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
			resp.Body.Close()
			ct := http.DetectContentType(body)
			if resp.StatusCode == 200 && (strings.HasPrefix(ct, "image/") || bytes.Contains(body[:min(len(body), 400)], []byte("<svg"))) {
				ic.body, ic.ct = body, ct
				if !strings.HasPrefix(ct, "image/") {
					ic.ct = "image/svg+xml"
				}
			}
		}
		brIconMu.Lock()
		if len(brIcons) > 300 {
			clear(brIcons)
		}
		brIcons[u] = ic
		brIconMu.Unlock()
	}
	if ic.body == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", cmp.Or(ic.ct, "image/x-icon"))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(ic.body)
}
