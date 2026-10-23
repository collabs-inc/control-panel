package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAITaskCancel(t *testing.T) {
	e := newTestServer(t)
	h := fakeHermes(t)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("k\n"), 0o600)
	e.s.ai = newAssistant(h.URL, key, filepath.Join(t.TempDir(), "g.json"))
	e.s.ai.tasksFile = filepath.Join(t.TempDir(), "tasks.json")
	board := `[{"id": 1, "title": "Calendar", "done_when": "x", "status": "blocked", "progress": 40, "created": 1, "updated": 2, "log": [{"t": 2, "status": "blocked", "progress": 40}], "future_field": true},
 {"id": 2, "title": "KPI tab", "done_when": "y", "status": "done", "progress": 100, "created": 1, "updated": 3},
 {"id": 3, "title": "Heartbeat", "done_when": "z", "status": "in_progress", "progress": 10, "created": 1, "updated": 4}]`
	os.WriteFile(e.s.ai.tasksFile, []byte(board), 0o600)
	cancel := func(body string) (int, string) {
		w := httptest.NewRecorder()
		e.s.aiTaskCancel(w, httptest.NewRequest("POST", "/api/ai/tasks/cancel", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}

	// a done one: off the board, JARVIS isn't bothered
	if code, body := cancel(`{"id": 2}`); code != 200 || !strings.Contains(body, `"told":""`) {
		t.Fatalf("done: %d %s", code, body)
	}
	b, _ := os.ReadFile(e.s.ai.tasksFile)
	if strings.Contains(string(b), "KPI tab") || !strings.Contains(string(b), `"future_field": true`) || !strings.Contains(string(b), `"log": [`) {
		t.Fatalf("the rest must stay as vm-agent wrote it:\n%s", b)
	}
	// an open one: JARVIS is told to stop (a new panel message: nothing is running)
	if code, body := cancel(`{"id": 3}`); code != 200 || !strings.Contains(body, `"told":"sent"`) {
		t.Fatalf("open: %d %s", code, body)
	}
	e.s.ai.mu.Lock()
	said := ""
	if e.s.ai.live != nil {
		said = e.s.ai.live.User
	}
	e.s.ai.mu.Unlock()
	if !strings.HasPrefix(said, "Cancel task #3 (Heartbeat)") {
		t.Fatalf("JARVIS was told %q", said)
	}
	for i := 0; i < 100; i++ { // let the fake turn end
		e.s.ai.mu.Lock()
		idle := e.s.ai.live == nil
		e.s.ai.mu.Unlock()
		if idle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := cancel(`{"id": 3}`); code != 404 {
		t.Fatalf("twice: %d", code)
	}
	if code, _ := cancel(`{"id": 0}`); code != 400 {
		t.Fatalf("no id: %d", code)
	}
	if a, _ := os.ReadFile(e.audit); !strings.Contains(string(a), "assistant-task-cancel") {
		t.Fatal("not audited")
	}

	// Mark done: done at 100% with the step in its timeline; the rest of the task as vm-agent wrote it
	w := httptest.NewRecorder()
	e.s.aiTaskDone(w, httptest.NewRequest("POST", "/api/ai/tasks/done", strings.NewReader(`{"id": 1}`)))
	if w.Code != 200 {
		t.Fatalf("done: %d %s", w.Code, w.Body)
	}
	var after []map[string]any
	b, _ = os.ReadFile(e.s.ai.tasksFile)
	json.Unmarshal(b, &after)
	got := after[0]
	log, _ := got["log"].([]any)
	last, _ := log[len(log)-1].(map[string]any)
	if len(after) != 1 || got["status"] != "done" || got["progress"] != 100.0 || got["future_field"] != true || got["title"] != "Calendar" ||
		len(log) != 2 || last["status"] != "done" || last["note"] != "Marked done by the owner in the panel" {
		t.Fatalf("after Mark done: %s", b)
	}
	w = httptest.NewRecorder()
	e.s.aiTaskDone(w, httptest.NewRequest("POST", "/api/ai/tasks/done", strings.NewReader(`{"id": 9}`)))
	if w.Code != 404 {
		t.Fatalf("a task that isn't there: %d", w.Code)
	}
}
