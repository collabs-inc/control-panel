package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Artifacts: the files a session wrote or edited (Write/Edit/MultiEdit/NotebookEdit), or handed over by
// naming them in a message (a script's report), read from its transcript and its subagents' transcripts,
// with where they stand on disk now.

type Artifact struct {
	Path      string `json:"path"`
	Created   bool   `json:"created"` // its first touch in the session was a Write
	Edits     int    `json:"edits"`
	Add       int    `json:"add"`
	Del       int    `json:"del"`
	Last      int64  `json:"last"`
	Exists    bool   `json:"exists"`
	Size      int64  `json:"size"`
	InTree    bool   `json:"inTree"`              // inside the session's checkout (its worktree)
	Mentioned bool   `json:"mentioned,omitempty"` // only named in a message, never written by a tool
	first     int64
}

type artScan struct {
	off   int64
	files map[string]*Artifact
}

// artifactCache keeps one incremental scan per transcript file: only appended lines are read.
type artifactCache struct {
	mu    sync.Mutex
	scans map[string]*artScan
}

var editToolNames = [][]byte{[]byte(`"name":"Write"`), []byte(`"name":"Edit"`), []byte(`"name":"MultiEdit"`), []byte(`"name":"NotebookEdit"`)}

func (c *artifactCache) scan(path string) map[string]*Artifact {
	sc := c.scans[path]
	fi, err := os.Stat(path)
	if sc == nil || (err == nil && fi.Size() < sc.off) { // new, or rewritten
		sc = &artScan{files: map[string]*Artifact{}}
		c.scans[path] = sc
	}
	if err != nil || fi.Size() == sc.off {
		return sc.files
	}
	f, err := os.Open(path)
	if err != nil {
		return sc.files
	}
	defer f.Close()
	f.Seek(sc.off, 0)
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // a partial last line waits for the next scan
		}
		sc.off += int64(len(line))
		if !bytes.Contains(line, []byte(`"tool_use"`)) {
			if bytes.Contains(line, []byte(`"type":"assistant"`)) && bytes.Contains(line, []byte(`"type":"text"`)) {
				sc.mentions(line)
			}
			continue
		}
		hit := false
		for _, n := range editToolNames {
			hit = hit || bytes.Contains(line, n)
		}
		var e tEntry
		if !hit || json.Unmarshal(line, &e) != nil {
			continue
		}
		t := unixOf(e.Timestamp)
		_, bs := blocks(e.Message.Content)
		for _, b := range bs {
			p, _, add, del, ok := editDiff(b.Name, b.Input)
			if b.Type != "tool_use" || !ok || p == "" {
				continue
			}
			a := sc.files[p]
			if a == nil {
				a = &Artifact{Path: p, Created: b.Name == "Write", first: t}
				sc.files[p] = a
			}
			a.Edits++
			a.Add += add
			a.Del += del
			a.Last = max(a.Last, t)
		}
	}
	return sc.files
}

// mentions records the documents an assistant message names (see mentionedFiles).
func (sc *artScan) mentions(line []byte) {
	var e tEntry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	t := unixOf(e.Timestamp)
	_, bs := blocks(e.Message.Content)
	for _, b := range bs {
		if b.Type != "text" {
			continue
		}
		for _, p := range mentionedFiles(b.Text, e.Cwd, homeDir, t) {
			if a := sc.files[p]; a != nil {
				a.Last = max(a.Last, t)
			} else {
				sc.files[p] = &Artifact{Path: p, Mentioned: true, Last: t, first: t}
			}
		}
	}
}

// list merges the session transcript's and its subagents' files; root is the session's checkout.
func (c *artifactCache) list(transcript, root string) []Artifact {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scans == nil {
		c.scans = map[string]*artScan{}
	}
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents", "agent-*.jsonl"))
	merged := map[string]*Artifact{}
	for _, p := range append([]string{transcript}, subs...) {
		for k, a := range c.scan(p) {
			m := merged[k]
			if m == nil {
				cp := *a
				merged[k] = &cp
				continue
			}
			m.Edits, m.Add, m.Del, m.Last = m.Edits+a.Edits, m.Add+a.Add, m.Del+a.Del, max(m.Last, a.Last)
			m.Mentioned = m.Mentioned && a.Mentioned
			if a.first < m.first {
				m.first, m.Created = a.first, a.Created
			}
		}
	}
	out := make([]Artifact, 0, len(merged))
	for _, a := range merged {
		if fi, err := os.Stat(a.Path); err == nil && !fi.IsDir() {
			a.Exists, a.Size = true, fi.Size()
		}
		a.InTree = root != "" && strings.HasPrefix(a.Path, root+"/")
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Last != out[j].Last {
			return out[i].Last > out[j].Last
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// ---- endpoints ----

func (s *server) paneArtifacts(r *http.Request) (TPane, []Artifact, bool) {
	session, pane, err := paneParams(r)
	if err != nil {
		return TPane{}, nil, false
	}
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil || p.Agent.path == "" {
		return TPane{}, nil, false
	}
	root := ""
	if p.Git != nil {
		root = p.Git.Root
	}
	return p, s.arts.list(p.Agent.path, root), true
}

func (s *server) paneFiles(w http.ResponseWriter, r *http.Request) {
	p, files, ok := s.paneArtifacts(r)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no session files for this pane"})
		return
	}
	var g GitInfo
	if p.Git != nil {
		g = *p.Git
	}
	writeJSON(w, 200, map[string]any{"files": files, "root": g.Root, "branch": g.Branch, "worktree": g.Worktree})
}

var fileTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".svg": "image/svg+xml", ".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8", ".pdf": "application/pdf",
}

// paneFile shows one of the session's own files: as JSON (kind + text) for the viewer, or raw
// (images, sandboxed HTML/SVG, downloads). Anything not in the session's list is a 404.
func (s *server) paneFile(w http.ResponseWriter, r *http.Request) {
	_, files, ok := s.paneArtifacts(r)
	want := r.URL.Query().Get("path")
	found := false
	for _, a := range files {
		found = found || (ok && a.Path == want)
	}
	for _, q := range s.secretPaths() { // a message naming ~/.ssh/… doesn't make it downloadable
		found = found && want != q && !strings.HasPrefix(want, q+"/")
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(want)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	ext := strings.ToLower(filepath.Ext(want))
	if r.URL.Query().Get("raw") == "1" {
		buf := make([]byte, min(fi.Size(), 25<<20))
		n, _ := f.Read(buf)
		h := w.Header()
		ct, known := fileTypes[ext]
		if !known {
			ct = "text/plain; charset=utf-8"
			h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+strings.ReplaceAll(filepath.Base(want), " ", "%20"))
		}
		// HTML and SVG render in an opaque origin: their scripts and web fonts work, but they can't read the
		// panel's cookies or pages, can't call home (connect-src), and the panel rejects cross-site actions.
		if ext == ".html" || ext == ".htm" || ext == ".svg" {
			h.Set("Content-Security-Policy", "sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox; default-src 'none'; "+
				"script-src 'unsafe-inline' https:; style-src 'unsafe-inline' https: data:; img-src https: data: blob:; font-src https: data:; "+
				"media-src https: data:; connect-src 'none'; frame-ancestors 'self'")
			h.Set("X-Frame-Options", "SAMEORIGIN")
		}
		h.Set("Content-Type", ct)
		h.Set("Cache-Control", "no-store")
		w.Write(buf[:n])
		return
	}
	buf := make([]byte, min(fi.Size(), 1<<20))
	n, _ := f.Read(buf)
	buf = buf[:n]
	kind := "text"
	switch {
	case strings.HasPrefix(fileTypes[ext], "image/"):
		kind = "image"
	case ext == ".md" || ext == ".markdown":
		kind = "markdown"
	case ext == ".html" || ext == ".htm":
		kind = "html"
	case bytes.IndexByte(buf[:min(len(buf), 8192)], 0) >= 0 || ext == ".pdf":
		kind = "binary"
	}
	out := map[string]any{"path": want, "kind": kind, "size": fi.Size(), "truncated": fi.Size() > int64(n)}
	if kind != "image" && kind != "binary" {
		out["text"] = string(bytes.ToValidUTF8(buf, []byte("�")))
	}
	writeJSON(w, 200, out)
}
