package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The panel chat's Hermes session and model. A new conversation is a new Hermes session ("panel-<time>"):
// what came before stays in the chat above a divider but no longer goes to the model. That is the way out
// when a conversation's history keeps the model refusing every turn. The model the panel chat uses can be
// picked here as well (Hermes takes it per request; "" = Hermes' own default).

type aiPanel struct {
	Session string    `json:"session"`
	Model   string    `json:"model,omitempty"`
	Resets  []aiReset `json:"resets,omitempty"` // where each new conversation began, for the chat's dividers
}

type aiReset struct {
	Session string  `json:"session"`
	T       float64 `json:"t"`
}

var aiModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{1,100}$`)

// loadPanel reads the saved session and model (the first one, "panel", when there's nothing saved).
func (a *assistant) loadPanel() {
	if b, err := os.ReadFile(a.stateFile); err == nil {
		json.Unmarshal(b, &a.panel)
	}
	if a.panel.Session == "" {
		a.panel.Session = aiPanelSession
	}
	a.state.Pick = a.panel.Model
}

func (a *assistant) savePanel() error { // a.mu held
	if a.stateFile == "" {
		return nil
	}
	b, _ := json.Marshal(a.panel)
	tmp := a.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.stateFile)
}

// newChat starts the panel chat over on a new Hermes session; a turn still running on the old one stops,
// and what was queued for it is dropped.
func (a *assistant) newChat() (string, error) {
	a.mu.Lock()
	a.queue = nil
	id := aiPanelSession + "-" + time.Now().Format("20060102-150405")
	a.panel.Session = id
	a.panel.Resets = append(a.panel.Resets, aiReset{Session: id, T: float64(time.Now().UnixMilli()) / 1000})
	err := a.savePanel()
	a.rebuild()
	a.bump()
	a.mu.Unlock()
	a.stop()
	return id, err
}

// setModel picks the model the panel chat uses from its next turn on ("" = Hermes' default).
func (a *assistant) setModel(m string) error {
	if m != "" && !aiModelRe.MatchString(m) {
		return errors.New("not a model id")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.panel.Model, a.state.Pick = m, m
	a.bump()
	return a.savePanel()
}

// aiSession: POST starts a new conversation.
func (s *server) aiSession(w http.ResponseWriter, r *http.Request) {
	id, err := s.ai.newChat()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("assistant-new-chat", id, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "session": id})
}

// aiModel: GET the panel chat's model and the ones Hermes lists for its provider; POST {"model"} picks one.
func (s *server) aiModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var b struct{ Model string }
		if err := readJSONN(r, &b, 1<<10); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad request"})
			return
		}
		if err := s.ai.setModel(strings.TrimSpace(b.Model)); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		s.audit.add("assistant-model", cmp.Or(b.Model, "default"), clientIP(r))
	}
	var cache map[string]struct {
		Models []string `json:"models"`
	}
	b, _ := os.ReadFile(filepath.Join(s.cfg.home, ".hermes/provider_models_cache.json"))
	json.Unmarshal(b, &cache)
	s.ai.mu.Lock()
	pick := s.ai.panel.Model
	s.ai.mu.Unlock()
	writeJSON(w, 200, map[string]any{"model": pick, "models": cache["anthropic"].Models})
}
