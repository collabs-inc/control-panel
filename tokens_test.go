package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func usageLine(ts time.Time, id string, in, out, cw, cr int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`+"\n",
		ts.UTC().Format(time.RFC3339Nano), id, in, out, cw, cr)
}

func TestTokensCountEachMessageOnceAndPickUpWhereTheyLeftOff(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-workspace-home-repos-alven-estate-agent")
	os.MkdirAll(filepath.Join(proj, "sid-a", "subagents"), 0o755)
	now := time.Date(2026, 10, 7, 20, 30, 0, 0, time.UTC)
	main := filepath.Join(proj, "sid-a.jsonl")
	os.WriteFile(main, []byte(
		`{"type":"user","message":{"content":"go"}}`+"\n"+
			usageLine(now.Add(-2*time.Hour), "msg_1", 10, 5, 100, 1000)+ // one message, written as two entries:
			usageLine(now.Add(-2*time.Hour), "msg_1", 10, 7, 100, 1000)+ // the last usage stands
			usageLine(now.Add(-30*time.Hour), "msg_0", 1, 1, 0, 0)+ // older than a day: in the graph, not "by agent"
			usageLine(now, "msg_2", 2, 2, 0, 0)[:40], // still being written
	), 0o600)
	os.WriteFile(filepath.Join(proj, "sid-a", "subagents", "agent-x.jsonl"), []byte(usageLine(now.Add(-time.Hour), "msg_s", 3, 3, 0, 0)), 0o600)

	state := filepath.Join(dir, "tokens.json")
	tk := newTokScan(filepath.Join(dir, "projects"), state)
	tk.scan(now)
	v := tk.view(now, func(proj, sid string) string { return proj + "/" + sid })
	sum := tokUse{}
	for _, h := range v.Hours {
		sum = sum.add(tokUse{In: h[1], Out: h[2], CW: h[3], CR: h[4]}, 1)
	}
	if want := (tokUse{In: 14, Out: 11, CW: 100, CR: 1000}); sum != want {
		t.Fatalf("hours add up to %+v, want %+v (msg_1 once, with its last usage; the half line not yet)", sum, want)
	}
	if len(v.Agents) != 1 || v.Agents[0].Label != "alven-estate-agent/sid-a" || v.Agents[0].Tokens != 10+7+100+1000+3+3 {
		t.Fatalf("agents = %+v (the subagent counts for its session; the 30 h old message doesn't)", v.Agents)
	}

	// the line finishes and another message comes; a restart reads on from where the scan stopped
	f, _ := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(usageLine(now, "msg_2", 2, 2, 0, 0)[40:] + usageLine(now, "msg_3", 1, 0, 0, 0))
	f.Close()
	tk2 := newTokScan(filepath.Join(dir, "projects"), state)
	tk2.scan(now.Add(time.Minute))
	total := int64(0)
	for _, h := range tk2.view(now.Add(time.Minute), nil).Hours {
		total += h[1] + h[2] + h[3] + h[4]
	}
	if want := int64(14 + 11 + 100 + 1000 + 4 + 1); total != want {
		t.Fatalf("after a restart: %d tokens, want %d (nothing counted twice)", total, want)
	}
}
