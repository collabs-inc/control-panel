package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Check struct {
	Level  string `json:"level"` // ok, warn, bad
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type Listen struct {
	Proto  string `json:"proto"`
	Addr   string `json:"addr"`
	Port   int    `json:"port"`
	PID    int    `json:"pid"`
	Proc   string `json:"proc"`
	Public bool   `json:"public"`
}

type Conn struct {
	Remote    string `json:"remote"`
	LocalPort int    `json:"localPort"`
	PID       int    `json:"pid"`
	Proc      string `json:"proc"`
}

type RemoteCount struct {
	Addr  string `json:"addr"`
	N     int    `json:"n"`
	Procs string `json:"procs"`
}

type Key struct {
	Type    string `json:"type"`
	FP      string `json:"fp"`
	Comment string `json:"comment"`
}

type Security struct {
	Checks   []Check       `json:"checks"`
	Listens  []Listen      `json:"listens"`
	Inbound  []Conn        `json:"inbound"`
	Outbound []RemoteCount `json:"outbound"`
	Keys     []Key         `json:"keys"`
	Loose    []string      `json:"loose"`
	Audit    []auditEvent  `json:"audit"`
	Bad      int           `json:"bad"`
}

type secEnv struct {
	home, ciDir, panelAddr     string
	accessOn, pinSet, tunnelUp bool
	audit                      *auditLog
	fails24h                   int
	lockedFor                  time.Duration
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && !a.IsLoopback() && !a.IsPrivate() && !a.IsLinkLocalUnicast() &&
		!a.IsUnspecified() && !a.IsMulticast() && !cgnat.Contains(a)
}

// fingerprint parses an authorized_keys line (options allowed) into its SHA256 fingerprint, as ssh-keygen -l prints it.
func fingerprint(line string) (Key, bool) {
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		t := f[i]
		if !strings.HasPrefix(t, "ssh-") && !strings.HasPrefix(t, "ecdsa-") && !strings.HasPrefix(t, "sk-") {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(f[i+1])
		if err != nil || len(blob) == 0 {
			return Key{}, false
		}
		sum := sha256.Sum256(blob)
		return Key{Type: t, FP: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), Comment: strings.Join(f[i+2:], " ")}, true
	}
	return Key{}, false
}

// looseFiles returns "path (mode)" for each existing file readable by other users.
func looseFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err == nil && fi.Mode().Perm()&0o004 != 0 {
			out = append(out, fmt.Sprintf("%s (%o)", p, fi.Mode().Perm()))
		}
	}
	return out
}

var secretName = regexp.MustCompile(`(?i)(token|secret|credential|password|passwd|\.pem$|\.key$|^\.env$|auth\.json$|hosts\.yml$|^id_[a-z0-9_]+$)`)

// secretCandidates lists files on this box that hold credentials.
func secretCandidates(home, ciDir string) []string {
	var out []string
	add := func(p string) {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	ssh, _ := os.ReadDir(filepath.Join(home, ".ssh"))
	for _, e := range ssh {
		n := e.Name()
		if !strings.HasSuffix(n, ".pub") && !strings.HasPrefix(n, "known_hosts") && n != "config" && n != "authorized_keys" {
			add(filepath.Join(home, ".ssh", n))
		}
	}
	for _, p := range []string{".claude/.credentials.json", ".git-credentials", ".netrc", ".docker/config.json",
		".npmrc", ".pypirc", ".aws/credentials", ".codex/auth.json", ".config/control-panel/pin"} {
		add(filepath.Join(home, p))
	}
	cf, _ := os.ReadDir(filepath.Join(home, ".cloudflared"))
	for _, e := range cf {
		add(filepath.Join(home, ".cloudflared", e.Name()))
	}
	cfg := filepath.Join(home, ".config")
	filepath.WalkDir(cfg, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, cfg), "/") >= 3 {
			return filepath.SkipDir
		}
		if !d.IsDir() && secretName.MatchString(d.Name()) && p != filepath.Join(cfg, "control-panel/pin") {
			add(p)
		}
		return nil
	})
	creds, _ := filepath.Glob(filepath.Join(ciDir, "*", ".credentials*"))
	for _, p := range creds {
		add(p)
	}
	return out
}

func secScan(sock sockScan, procName func(pid int) string, env secEnv) Security {
	var s Security
	tilde := func(p string) string { return strings.Replace(p, env.home, "~", 1) }

	listening := map[uint16]bool{}
	seen := map[string]bool{}
	for _, e := range sock.entries {
		isListen := (e.Proto[:3] == "tcp" && e.State == "0A") || (e.Proto[:3] == "udp" && e.State == "07" && e.Remote.Port() == 0)
		if !isListen {
			continue
		}
		listening[e.Local.Port()] = true
		pub := !e.Local.Addr().IsLoopback()
		k := fmt.Sprintf("%s/%d/%v", e.Proto[:3], e.Local.Port(), pub)
		if seen[k] {
			continue
		}
		seen[k] = true
		pid := sock.inodePID[e.Inode]
		proc := procName(pid)
		if pid == 0 && e.UID == 0 {
			proc = "root" // another user's socket: its fds aren't readable to us
		}
		s.Listens = append(s.Listens, Listen{Proto: e.Proto[:3], Addr: e.Local.Addr().String(), Port: int(e.Local.Port()), PID: pid, Proc: proc, Public: pub})
	}
	sort.Slice(s.Listens, func(i, j int) bool {
		a, b := s.Listens[i], s.Listens[j]
		if a.Public != b.Public {
			return a.Public
		}
		return a.Port < b.Port
	})

	out := map[string]*RemoteCount{}
	outProcs := map[string]map[string]bool{}
	for _, e := range sock.entries {
		if e.Proto[:3] != "tcp" || e.State != "01" || !isPublic(e.Remote.Addr()) {
			continue
		}
		pid := sock.inodePID[e.Inode]
		ip := e.Remote.Addr().Unmap().String()
		if listening[e.Local.Port()] {
			s.Inbound = append(s.Inbound, Conn{Remote: ip, LocalPort: int(e.Local.Port()), PID: pid, Proc: procName(pid)})
			continue
		}
		rc := out[ip]
		if rc == nil {
			rc = &RemoteCount{Addr: ip}
			out[ip], outProcs[ip] = rc, map[string]bool{}
		}
		rc.N++
		if n := procName(pid); n != "" {
			outProcs[ip][n] = true
		}
	}
	for ip, rc := range out {
		var names []string
		for n := range outProcs[ip] {
			names = append(names, n)
		}
		sort.Strings(names)
		rc.Procs = strings.Join(names, ", ")
		s.Outbound = append(s.Outbound, *rc)
	}
	sort.Slice(s.Outbound, func(i, j int) bool {
		return s.Outbound[i].N > s.Outbound[j].N || (s.Outbound[i].N == s.Outbound[j].N && s.Outbound[i].Addr < s.Outbound[j].Addr)
	})
	s.Outbound = s.Outbound[:min(len(s.Outbound), 15)]

	if b, err := os.ReadFile(filepath.Join(env.home, ".ssh", "authorized_keys")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, ok := fingerprint(line); ok {
				s.Keys = append(s.Keys, k)
			}
		}
	}
	for _, l := range looseFiles(secretCandidates(env.home, env.ciDir)) {
		s.Loose = append(s.Loose, tilde(l))
	}
	if env.audit != nil {
		s.Audit = env.audit.list()
		s.Audit = s.Audit[:min(len(s.Audit), 50)]
	}

	add := func(ok bool, badLevel, title, okDetail, badDetail string) {
		c := Check{Level: "ok", Title: title, Detail: okDetail}
		if !ok {
			c.Level, c.Detail = badLevel, badDetail
		}
		s.Checks = append(s.Checks, c)
	}
	host, _, _ := net.SplitHostPort(env.panelAddr)
	hostIP, _ := netip.ParseAddr(host)
	var pubPorts []string
	for _, l := range s.Listens {
		if l.Public {
			pubPorts = append(pubPorts, fmt.Sprintf("%s:%d (%s)", l.Addr, l.Port, orDash(l.Proc)))
		}
	}
	add(env.accessOn, "bad", "Cloudflare Access enforced",
		"every request must carry a valid Access token", "running with -insecure-no-access: anyone who reaches the port sees the PIN screen")
	add(hostIP.IsLoopback(), "bad", "Panel bound to localhost",
		env.panelAddr+" — reachable only through the tunnel", env.panelAddr+" is reachable from the network directly")
	add(env.pinSet, "bad", "PIN set", "PIN hash in ~/.config/control-panel/pin", "run `control-panel set-pin` on the VM")
	add(env.tunnelUp, "warn", "Tunnel running", "cloudflared is up", "cloudflared is not running: panel unreachable from outside")
	add(len(pubPorts) == 0, "warn", "Ports open beyond localhost",
		"nothing listens on a public interface", strings.Join(pubPorts, ", ")+" — reachability is decided by the Cube/Fly edge")
	add(len(s.Inbound) == 0, "warn", "Inbound public connections",
		"none right now", fmt.Sprintf("%d connection(s) from public IPs", len(s.Inbound)))
	add(len(s.Loose) == 0, "warn", "World-readable secrets",
		"credential files are private", fmt.Sprintf("%d file(s) readable by other users — chmod 600", len(s.Loose)))
	switch {
	case env.lockedFor > 0:
		s.Checks = append(s.Checks, Check{"bad", "Failed PIN attempts (24 h)", fmt.Sprintf("login locked for %s after repeated wrong PINs", env.lockedFor.Round(time.Second))})
	case env.fails24h > 0:
		s.Checks = append(s.Checks, Check{"warn", "Failed PIN attempts (24 h)", fmt.Sprintf("%d wrong PIN(s)", env.fails24h)})
	default:
		s.Checks = append(s.Checks, Check{"ok", "Failed PIN attempts (24 h)", "none"})
	}
	s.Checks = append(s.Checks, Check{"ok", "SSH authorized keys", fmt.Sprintf("%d key(s)", len(s.Keys))})
	for _, c := range s.Checks {
		if c.Level == "bad" {
			s.Bad++
		}
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
