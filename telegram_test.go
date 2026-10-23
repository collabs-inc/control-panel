package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAITelegramSettings(t *testing.T) {
	e := newTestServer(t)
	env := filepath.Join(t.TempDir(), ".env")
	before := "# Hermes\nANTHROPIC_API_KEY=sk-ant-dummy\nAPI_SERVER_PORT=8642\n\n# Telegram (values set by owner in the panel)\nTELEGRAM_BOT_TOKEN=\nTELEGRAM_ALLOWED_USERS=\n"
	os.WriteFile(env, []byte(before), 0o600)
	e.s.cfg.hermesEnv = env
	call := func(method, body string) (int, string) {
		w := httptest.NewRecorder()
		e.s.aiTelegram(w, httptest.NewRequest(method, "/api/ai/telegram", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}

	if code, body := call("GET", ""); code != 200 || !strings.Contains(body, `"token":false`) || !strings.Contains(body, `"users":""`) {
		t.Fatalf("empty: %d %s", code, body)
	}
	for _, bad := range []string{`{"token":"not-a-token"}`, `{"users":"me"}`, `{"users":"12; rm -rf"}`, `{}`} {
		if code, _ := call("POST", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if b, _ := os.ReadFile(env); string(b) != before {
		t.Fatalf("a rejected save touched the file:\n%s", b)
	}

	token := "123456789:AAFakeTokenForTests_abcdefghijklmnopq"
	code, body := call("POST", `{"token":"`+token+`","users":"987654321"}`)
	if code != 200 || strings.Contains(body, token) || !strings.Contains(body, `"token":true`) || !strings.Contains(body, `"users":"987654321"`) || !strings.Contains(body, `"home":"987654321"`) {
		t.Fatalf("save: %d %s", code, body)
	}
	b, _ := os.ReadFile(env)
	want := "# Hermes\nANTHROPIC_API_KEY=sk-ant-dummy\nAPI_SERVER_PORT=8642\n\n# Telegram (values set by owner in the panel)\nTELEGRAM_BOT_TOKEN=" + token + "\nTELEGRAM_ALLOWED_USERS=987654321\nTELEGRAM_HOME_CHANNEL=987654321\n"
	if string(b) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(env); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if code, body := call("GET", ""); code != 200 || strings.Contains(body, token) || !strings.Contains(body, `"token":true`) {
		t.Fatalf("token echoed or lost: %s", body)
	}
	// changing the user id later keeps the token and an existing home channel
	call("POST", `{"users":"111222333, 444555666"}`)
	b, _ = os.ReadFile(env)
	if !strings.Contains(string(b), "TELEGRAM_BOT_TOKEN="+token+"\n") || !strings.Contains(string(b), "TELEGRAM_ALLOWED_USERS=111222333,444555666\n") || !strings.Contains(string(b), "TELEGRAM_HOME_CHANNEL=987654321\n") {
		t.Fatalf("second save:\n%s", b)
	}
	if a, _ := os.ReadFile(e.audit); strings.Contains(string(a), token) || !strings.Contains(string(a), "assistant-telegram") {
		t.Fatalf("audit: %s", a)
	}
}

func TestGatewayPIDs(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, cmd string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "cmdline"), []byte(strings.ReplaceAll(cmd, " ", "\x00")), 0o644)
	}
	write("100", "python3 -I -c launcher gateway run --external-supervisor --replace")
	write("101", "python3 -I -c launcher dashboard --host 127.0.0.1")
	write("102", "python3 -I -c launcher gateway run") // not run-agent.sh's: left alone
	write("self", "x")
	if got := gatewayPIDs(proc); len(got) != 1 || got[0] != 100 {
		t.Fatalf("got %v", got)
	}
}
