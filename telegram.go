package main

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// The Telegram channel's settings, set from the Assistant's Connections: the bot token and the owner's user
// id, written into Hermes' .env (mode 600, only those lines touched). The token is write-only: reading says
// whether one is set, never what it is. The gateway reads them when it restarts.

var (
	tgTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{30,}$`)
	tgUsersRe = regexp.MustCompile(`^\d{3,}(,\d{3,})*$`)
)

// envRead: the file's KEY=value pairs (the last one wins, like a shell). A missing file reads as empty.
func envRead(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			out[strings.TrimSpace(strings.TrimPrefix(k, "export "))] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// envSet rewrites the lines for these keys (adding the ones that aren't there) and keeps every other line as
// it was. Written to a temp file beside it and renamed over it, mode 600.
func envSet(path string, kv map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}
	done := map[string]bool{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		if v, want := kv[k]; ok && want {
			lines[i] = k + "=" + v
			done[k] = true
		}
	}
	for _, k := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_USERS", "TELEGRAM_HOME_CHANNEL"} { // a stable order
		if v, want := kv[k]; want && !done[k] {
			lines = append(lines, k+"="+v)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env.panel-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *server) aiTelegram(w http.ResponseWriter, r *http.Request) {
	path := s.cfg.hermesEnv
	if r.Method == "POST" {
		var b struct {
			Token *string `json:"token"`
			Users *string `json:"users"`
		}
		if err := readJSON(r, &b); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad request"})
			return
		}
		kv := map[string]string{}
		if b.Token != nil && strings.TrimSpace(*b.Token) != "" {
			t := strings.TrimSpace(*b.Token)
			if !tgTokenRe.MatchString(t) {
				writeJSON(w, 400, map[string]any{"error": "that doesn't look like a bot token (123456789:AA… from @BotFather)"})
				return
			}
			kv["TELEGRAM_BOT_TOKEN"] = t
		}
		if b.Users != nil {
			u := strings.ReplaceAll(strings.TrimSpace(*b.Users), " ", "")
			if !tgUsersRe.MatchString(u) {
				writeJSON(w, 400, map[string]any{"error": "your user id is a number (ask @userinfobot); several: comma-separated"})
				return
			}
			kv["TELEGRAM_ALLOWED_USERS"] = u
			if envRead(path)["TELEGRAM_HOME_CHANNEL"] == "" { // fleet pings go to your DM
				kv["TELEGRAM_HOME_CHANNEL"] = strings.Split(u, ",")[0]
			}
		}
		if len(kv) == 0 {
			writeJSON(w, 400, map[string]any{"error": "nothing to save"})
			return
		}
		if err := envSet(path, kv); err != nil {
			writeJSON(w, 500, map[string]any{"error": "couldn't save: " + err.Error()})
			return
		}
		keys := []string{}
		for k := range kv {
			keys = append(keys, k)
		}
		s.audit.add("assistant-telegram", "set "+strings.Join(keys, ", "), clientIP(r)) // names only, never values
	}
	env := envRead(path)
	writeJSON(w, 200, map[string]any{"token": env["TELEGRAM_BOT_TOKEN"] != "", "users": env["TELEGRAM_ALLOWED_USERS"], "home": env["TELEGRAM_HOME_CHANNEL"]})
}

// gatewayPIDs: the assistant's gateway processes, the ones run-agent.sh starts (`… gateway run --external-supervisor`),
// that this user owns.
func gatewayPIDs(proc string) []int {
	var out []int
	dirs, _ := os.ReadDir(proc)
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(proc, d.Name(), "cmdline"))
		cmd := strings.ReplaceAll(string(b), "\x00", " ")
		fi, err := os.Stat(filepath.Join(proc, d.Name()))
		if err != nil || !strings.Contains(cmd, " gateway run ") || !strings.Contains(cmd, "--external-supervisor") {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			out = append(out, pid)
		}
	}
	return out
}

// aiRestart restarts the assistant (to pick up new settings, like Telegram's): SIGTERM to its gateway, and
// run-agent.sh, which runs it in a loop, starts it again a couple of seconds later. Hermes' own restart would
// start a copy outside that loop.
func (s *server) aiRestart(w http.ResponseWriter, r *http.Request) {
	pids := gatewayPIDs("/proc")
	if len(pids) == 0 {
		writeJSON(w, 404, map[string]any{"error": "JARVIS isn't running: start projects/vm-agent/run-agent.sh"})
		return
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	s.audit.add("assistant-restart", "gateway pid "+strconv.Itoa(pids[0]), clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}
