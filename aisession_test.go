package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAINewChatAndModel(t *testing.T) {
	var mu sync.Mutex
	var turns []string            // session id and model of each turn Hermes got
	titles := map[string]string{} // like Hermes: an id and a title belong to one session only
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/sessions" {
			var b struct{ ID, Title string }
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			defer mu.Unlock()
			if _, ok := titles[b.ID]; ok {
				w.WriteHeader(409)
				return
			}
			for _, t := range titles {
				if t == b.Title {
					http.Error(w, `{"error": {"code": "invalid_title"}}`, 400)
					return
				}
			}
			titles[b.ID] = b.Title
			w.WriteHeader(201)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/stream") {
			mu.Lock()
			_, ok := titles[strings.Split(r.URL.Path, "/")[3]]
			mu.Unlock()
			if !ok {
				http.Error(w, "Session not found", 404)
				return
			}
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/stream") {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			turns = append(turns, fmt.Sprintf("%s %v", strings.Split(r.URL.Path, "/")[3], b["model"]))
			mu.Unlock()
			fmt.Fprint(w, "event: run.completed\ndata: {\"messages\":[1]}\n\n")
			return
		}
		w.WriteHeader(201)
	}))
	t.Cleanup(h.Close)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("k"), 0o600)
	state := filepath.Join(t.TempDir(), "assistant.json")
	a := newAssistant(h.URL, key, "")
	a.stateFile = state
	a.loadPanel()
	idle := func() {
		for i := 0; i < 200; i++ {
			a.mu.Lock()
			done := a.live == nil
			a.mu.Unlock()
			if done {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("turn never ended")
	}

	a.send("hi")
	idle()
	if err := a.setModel("claude-sonnet-5-5"); err != nil || a.setModel("rm -rf /") == nil {
		t.Fatal("model ids: a real one is taken, anything else isn't", err)
	}
	id, err := a.newChat()
	if err != nil || !strings.HasPrefix(id, "panel-") {
		t.Fatal(id, err)
	}
	a.send("again")
	idle()
	mu.Lock()
	got := fmt.Sprint(turns)
	mu.Unlock()
	if got != fmt.Sprintf("[panel <nil> %s claude-sonnet-5-5]", id) {
		t.Fatalf("turns went to %s", got)
	}
	a.mu.Lock()
	divider := slices.ContainsFunc(a.items, func(it aiItem) bool { return it.Role == "event" && it.Name == "new_chat" && it.K == "new:"+id })
	a.mu.Unlock()
	if !divider {
		t.Fatal("no divider where the chat started over")
	}
	if channelOf(hSession{ID: "agent-dev-1-23", Source: "api_server"}) != "agent" {
		t.Fatal("a Claude agent's conversation is the Agent channel")
	}
	if channelOf(hSession{ID: id, Source: "api_server"}) != "panel" || channelOf(hSession{ID: "panelx", Source: "api_server"}) != "" {
		t.Fatal("every panel conversation is the Panel chat, nothing else is")
	}
	// a restart keeps the conversation and the model
	b := newAssistant(h.URL, key, "")
	b.stateFile = state
	b.loadPanel()
	if b.panel.Session != id || b.panel.Model != "claude-sonnet-5-5" || b.state.Pick != "claude-sonnet-5-5" || len(b.panel.Resets) != 1 {
		t.Fatalf("after a restart: %+v", b.panel)
	}
	// Hermes' stand-in for a refused turn shows as an error in the chat
	its := toItems("panel", "panel", []hMessage{{ID: json.RawMessage(`1`), Role: "assistant", Kind: "failed_turn", Content: json.RawMessage(`"Your request was not processed."`), Timestamp: 1}})
	if len(its) != 1 || its[0].Role != "error" || its[0].Name != "failed_turn" {
		t.Fatalf("%+v", its)
	}
}
