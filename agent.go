package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The agent API: the panel's own handlers on a second, loopback-only listener, for the owner's
// assistant (projects/vm-agent, a Hermes agent whose MCP tools call this). A bearer token in a 0600
// file stands in for Access + login; every write is audited like the page's, from "agent".

func (s *server) agentHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/snapshot", s.agentSnapshot)
	mux.HandleFunc("GET /agent/pane/screen", s.agentScreen)
	mux.HandleFunc("GET /agent/pane/chat", s.agentChat)
	mux.HandleFunc("POST /agent/pane/input", s.paneInput)
	mux.HandleFunc("POST /agent/term/new", s.termNew)
	mux.HandleFunc("POST /agent/term/close", s.termClose)
	mux.HandleFunc("GET /agent/notes", s.notesList)
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, 401, map[string]any{"error": "bad token"})
			return
		}
		r.Header.Set("Cf-Connecting-Ip", "agent") // what clientIP puts in audit lines
		mux.ServeHTTP(w, r)
	})
}

func validAgentAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return errors.New("-agent-addr must be loopback")
	}
	return nil
}

// agentToken reads the token file, creating it (32 random bytes, hex, mode 0600) on first use.
func agentToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	rand.Read(buf)
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// look has the sampler run one tick for view as if a page showed it ("term" also refreshes the full
// pane snapshot that s.term.pane reads) and returns that view's data.
func (s *server) look(view string) (json.RawMessage, error) {
	c, done := s.subscribe(view)
	defer done()
	select {
	case msg := <-c.ch:
		var m struct {
			Data json.RawMessage `json:"data"`
		}
		err := json.Unmarshal(msg, &m)
		return m.Data, err
	case <-time.After(15 * time.Second):
		return nil, errors.New("the panel's sampler did not answer in 15 s")
	}
}

func (s *server) agentSnapshot(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if !views[view] || view == "ai" {
		writeJSON(w, 400, map[string]any{"error": "view must be res, ci, term or sec"})
		return
	}
	data, err := s.look(view)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// agentScreen: what the pane shows now, and the question on it if a TUI is blocked on one.
func (s *server) agentScreen(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if !s.sessionLive(session) {
		writeJSON(w, 404, map[string]any{"error": "no live session " + session})
		return
	}
	screen, err := s.z.dump(session, pane)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"screen": screen, "prompt": parsePrompt(screen)})
}

// agentChat: the last n items (default 30, at most 300) of the Claude transcript in a pane.
func (s *server) agentChat(w http.ResponseWriter, r *http.Request) {
	session, pane, err := paneParams(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	n := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil {
		n = min(max(v, 1), 300)
	}
	if _, err := s.look("term"); err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	p, ok := s.term.pane(session, pane)
	if !ok || p.Agent == nil || p.Agent.path == "" {
		writeJSON(w, 404, map[string]any{"error": "no Claude transcript for this pane"})
		return
	}
	fi, err := os.Stat(p.Agent.path)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	ls, _ := readBefore(p.Agent.path, fi.Size(), chatPage)
	items, title := parseChat(ls)
	writeJSON(w, 200, map[string]any{"title": title, "state": p.State, "items": lastItems(items, n)})
}
