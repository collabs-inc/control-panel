package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	// ssh-keygen -lf → 256 SHA256:W1hmbfYFyUzWapkTgmsS7A7j7bVoXRSOA/i9o9fcD+Y test@box (ED25519)
	k, ok := fingerprint(`no-pty,from="1.2.3.4" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGeXkuJnsWI7Q8WF6MjtTcHKrHdDeAuUR+/n0WhS/gjq test@box`)
	if !ok || k.Type != "ssh-ed25519" || k.FP != "SHA256:W1hmbfYFyUzWapkTgmsS7A7j7bVoXRSOA/i9o9fcD+Y" || k.Comment != "test@box" {
		t.Fatalf("got %+v ok=%v", k, ok)
	}
	for _, bad := range []string{"", "# comment", "ssh-ed25519 not-base64!!"} {
		if _, ok := fingerprint(bad); ok {
			t.Errorf("fingerprint(%q) accepted", bad)
		}
	}
}

func TestIsPublic(t *testing.T) {
	for s, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.5.4": false, "192.168.1.1": false,
		"fdaa:af:3451:a7b::2": false, "fe80::1": false, "0.0.0.0": false, "::": false, "100.100.1.1": false,
	} {
		if isPublic(netip.MustParseAddr(s)) != want {
			t.Errorf("isPublic(%s) != %v", s, want)
		}
	}
}

func TestLooseFiles(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.pem")
	shut := filepath.Join(dir, "shut.pem")
	os.WriteFile(open, nil, 0o644)
	os.WriteFile(shut, nil, 0o600)
	os.Chmod(open, 0o644) // umask-proof
	got := looseFiles([]string{open, shut, filepath.Join(dir, "missing")})
	if len(got) != 1 || !strings.HasPrefix(got[0], open) {
		t.Fatalf("got %v", got)
	}
}

func TestSecretCandidates(t *testing.T) {
	home := t.TempDir()
	ci := t.TempDir()
	for _, p := range []string{
		home + "/.ssh/id_ed25519", home + "/.ssh/id_ed25519.pub", home + "/.ssh/known_hosts",
		home + "/.config/gh/hosts.yml", home + "/.config/zellij/keybinds.kdl", home + "/.config/x/api_token",
		home + "/.cloudflared/cert.pem", ci + "/r1/.credentials", ci + "/r1/.runner",
	} {
		writeFile(t, p, "")
	}
	got := strings.Join(secretCandidates(home, ci), "\n")
	for _, want := range []string{"/.ssh/id_ed25519", "/.config/gh/hosts.yml", "/.config/x/api_token", "/.cloudflared/cert.pem", "/r1/.credentials"} {
		if !strings.Contains(got, want+"\n") && !strings.HasSuffix(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	for _, not := range []string{".pub", "known_hosts", "keybinds.kdl", ".runner"} {
		if strings.Contains(got, not) {
			t.Errorf("unexpected %s in:\n%s", not, got)
		}
	}
}

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestSecScan(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/.ssh/authorized_keys", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGeXkuJnsWI7Q8WF6MjtTcHKrHdDeAuUR+/n0WhS/gjq test@box\n")
	writeFile(t, home+"/.cloudflared/cert.pem", "x")
	os.Chmod(home+"/.cloudflared/cert.pem", 0o644)
	sock := sockScan{
		entries: []sockEntry{
			{Proto: "tcp", Local: ap("0.0.0.0:8080"), Remote: ap("0.0.0.0:0"), State: "0A", Inode: "1"},
			{Proto: "tcp6", Local: ap("[::]:8080"), Remote: ap("[::]:0"), State: "0A", Inode: "2"},
			{Proto: "tcp", Local: ap("127.0.0.1:7777"), Remote: ap("0.0.0.0:0"), State: "0A", Inode: "3"},
			{Proto: "tcp", Local: ap("0.0.0.0:22022"), Remote: ap("0.0.0.0:0"), State: "0A", Inode: "8", UID: 0},
			{Proto: "tcp", Local: ap("172.19.0.2:8080"), Remote: ap("8.8.8.8:5555"), State: "01", Inode: "4"},
			{Proto: "tcp", Local: ap("172.19.0.2:40000"), Remote: ap("1.1.1.1:443"), State: "01", Inode: "5"},
			{Proto: "tcp", Local: ap("172.19.0.2:40001"), Remote: ap("1.1.1.1:443"), State: "01", Inode: "6"},
			{Proto: "tcp", Local: ap("127.0.0.1:40002"), Remote: ap("127.0.0.1:7777"), State: "01", Inode: "7"},
		},
		inodePID: map[string]int{"1": 5, "2": 5, "3": 9, "4": 5, "5": 7, "6": 7},
	}
	names := map[int]string{5: "node", 7: "claude", 9: "control-panel"}
	audit := openAudit(filepath.Join(t.TempDir(), "a.jsonl"))
	audit.add("login", "ok", "1.2.3.4")
	s := secScan(sock, func(pid int) string { return names[pid] }, secEnv{
		home: home, ciDir: t.TempDir(), panelAddr: "127.0.0.1:7777",
		accessOn: true, pinSet: true, tunnelUp: false, audit: audit, fails24h: 2,
	})
	if len(s.Listens) != 3 || !s.Listens[0].Public || s.Listens[0].Port != 8080 || s.Listens[0].Proc != "node" ||
		s.Listens[1].Port != 22022 || s.Listens[1].Proc != "root" || s.Listens[2].Public {
		t.Fatalf("listens = %+v", s.Listens)
	}
	if len(s.Inbound) != 1 || s.Inbound[0].Remote != "8.8.8.8" || s.Inbound[0].LocalPort != 8080 {
		t.Fatalf("inbound = %+v", s.Inbound)
	}
	if len(s.Outbound) != 1 || s.Outbound[0].Addr != "1.1.1.1" || s.Outbound[0].N != 2 || s.Outbound[0].Procs != "claude" {
		t.Fatalf("outbound = %+v", s.Outbound)
	}
	if len(s.Keys) != 1 || len(s.Loose) != 1 || len(s.Audit) != 1 {
		t.Fatalf("keys=%v loose=%v audit=%v", s.Keys, s.Loose, s.Audit)
	}
	levels := map[string]string{}
	for _, c := range s.Checks {
		levels[c.Title] = c.Level
	}
	want := map[string]string{
		"Cloudflare Access enforced": "ok", "Panel bound to localhost": "ok", "PIN set": "ok",
		"Tunnel running": "warn", "Ports open beyond localhost": "warn", "Inbound public connections": "warn",
		"World-readable secrets": "warn", "Failed PIN attempts (24 h)": "warn",
	}
	for title, lvl := range want {
		if levels[title] != lvl {
			t.Errorf("%s = %q, want %q", title, levels[title], lvl)
		}
	}
	s = secScan(sockScan{}, func(int) string { return "" }, secEnv{home: home, panelAddr: "0.0.0.0:7777", lockedFor: time.Minute})
	levels = map[string]string{}
	for _, c := range s.Checks {
		levels[c.Title] = c.Level
	}
	for _, title := range []string{"Cloudflare Access enforced", "Panel bound to localhost", "PIN set", "Failed PIN attempts (24 h)"} {
		if levels[title] != "bad" {
			t.Errorf("%s = %q, want bad", title, levels[title])
		}
	}
}

// Behind Cube, the gate is the sign-in: no Access, PIN or tunnel check reports it as missing.
func TestSecScanBehindCube(t *testing.T) {
	s := secScan(sockScan{}, func(int) string { return "" }, secEnv{
		home: t.TempDir(), panelAddr: "127.0.0.1:7777", behindCube: true, audit: openAudit(filepath.Join(t.TempDir(), "a.jsonl")),
	})
	levels := map[string]string{}
	for _, c := range s.Checks {
		levels[c.Title] = c.Level
	}
	if levels["Cube signs visitors in"] != "ok" || levels["Panel bound to localhost"] != "ok" {
		t.Fatalf("checks = %+v", s.Checks)
	}
	for _, title := range []string{"Cloudflare Access enforced", "PIN set", "Tunnel running", "Failed PIN attempts (24 h)"} {
		if _, ok := levels[title]; ok {
			t.Errorf("%s reported behind Cube", title)
		}
	}
	if s.Bad != 0 {
		t.Errorf("bad = %d", s.Bad)
	}
}
