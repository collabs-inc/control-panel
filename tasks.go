package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

// Cancelling a task from the panel: it leaves JARVIS's board at once, and JARVIS is told to stop working on it.
// The board is vm-agent's (the Task model, written by mcp/vm_mcp.py's task_track); this takes the same lock.

var errNoTask = errors.New("no such task (already gone?)")

// editTask changes task id on the board under vm-agent's lock; change returns true to take it off the board.
// Every other entry, and every field of this one the panel doesn't know, stays as vm-agent wrote it.
func (a *assistant) editTask(id int, change func(t map[string]any) (drop bool)) (aiTask, error) {
	lock, err := os.OpenFile(a.tasksFile+".lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return aiTask{}, err
	}
	defer lock.Close() // releases the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return aiTask{}, err
	}
	b, err := os.ReadFile(a.tasksFile)
	if err != nil {
		return aiTask{}, errNoTask
	}
	var all []json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return aiTask{}, fmt.Errorf("the board can't be read: %w", err)
	}
	var hit aiTask
	kept := all[:0]
	for _, raw := range all {
		var t aiTask
		if hit.ID == 0 && json.Unmarshal(raw, &t) == nil && t.ID == id {
			hit = t
			var m map[string]any
			json.Unmarshal(raw, &m)
			if change(m) {
				continue
			}
			raw, _ = json.Marshal(m)
		}
		kept = append(kept, raw)
	}
	if hit.ID == 0 {
		return aiTask{}, errNoTask
	}
	out, _ := json.MarshalIndent(kept, "", " ")
	tmp := a.tasksFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return aiTask{}, err
	}
	return hit, os.Rename(tmp, a.tasksFile)
}

func (a *assistant) dropTask(id int) (aiTask, error) {
	return a.editTask(id, func(map[string]any) bool { return true })
}

// finishTask marks task id done, as the Task model has it: 100%, with the step in its timeline (last 30 kept).
func (a *assistant) finishTask(id int) (aiTask, error) {
	const note = "Marked done by the owner in the panel"
	now := float64(time.Now().UnixMilli()) / 1000
	return a.editTask(id, func(t map[string]any) bool {
		t["status"], t["progress"], t["updated"], t["note"] = "done", 100, now, note
		log, _ := t["log"].([]any)
		t["log"] = append(log[max(0, len(log)-29):], map[string]any{"t": now, "status": "done", "progress": 100, "note": note})
		return false
	})
}

// tell gets a note to JARVIS from the panel: into the panel turn it's running (Hermes' steer), else as a new
// message in the panel chat, queued behind a turn that is only starting. "" when none of that worked.
func (a *assistant) tell(msg string) string {
	a.mu.Lock()
	run := ""
	if a.live != nil {
		run = a.live.Run
	}
	a.mu.Unlock()
	if run != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if resp, err := a.do(ctx, "POST", "/v1/runs/"+url.PathEscape(run)+"/steer", map[string]any{"text": msg}); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return "steered"
			}
		}
	}
	switch queued, err := a.send(msg); {
	case err != nil:
		return ""
	case queued:
		return "queued"
	}
	return "sent"
}

// aiTaskCancel: the owner's "Cancel task" (an open one: JARVIS stops the work) or "Remove" (a done one).
func (s *server) aiTaskCancel(w http.ResponseWriter, r *http.Request) {
	var b struct{ ID int }
	if err := readJSONN(r, &b, 1<<10); err != nil || b.ID <= 0 {
		writeJSON(w, 400, map[string]any{"error": "which task?"})
		return
	}
	t, err := s.ai.dropTask(b.ID)
	if err != nil {
		writeJSON(w, cmpCode(err), map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("assistant-task-cancel", fmt.Sprintf("#%d %s (%s)", t.ID, trunc(t.Title, 80), t.Status), clientIP(r))
	told := ""
	if t.Status != "done" {
		told = s.ai.tell(fmt.Sprintf("Cancel task #%d (%s): I took it off the board in the panel. Stop any work on it "+
			"(agents you started for it, jobs, follow-ups you planned) and tell me briefly what you stopped. Don't put it back on the board.", t.ID, t.Title))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": t.Status, "told": told})
}

// aiTaskDone: the owner's "Mark done". JARVIS isn't messaged: the board is where it looks.
func (s *server) aiTaskDone(w http.ResponseWriter, r *http.Request) {
	var b struct{ ID int }
	if err := readJSONN(r, &b, 1<<10); err != nil || b.ID <= 0 {
		writeJSON(w, 400, map[string]any{"error": "which task?"})
		return
	}
	t, err := s.ai.finishTask(b.ID)
	if err != nil {
		writeJSON(w, cmpCode(err), map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("assistant-task-done", fmt.Sprintf("#%d %s", t.ID, trunc(t.Title, 80)), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func cmpCode(err error) int {
	if errors.Is(err, errNoTask) {
		return 404
	}
	return 500
}
