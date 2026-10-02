package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("control-panel", flag.ExitOnError)
	cfg := config{home: home}
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:7777", "listen address (must be loopback)")
	fs.StringVar(&cfg.cfTeam, "cf-team", "", "Cloudflare Zero Trust team name (<team>.cloudflareaccess.com)")
	fs.StringVar(&cfg.cfAUD, "cf-aud", "", "Cloudflare Access application AUD tag")
	fs.StringVar(&cfg.pinFile, "pin-file", filepath.Join(home, ".config/control-panel/pin"), "PIN hash file")
	fs.StringVar(&cfg.auditFile, "audit-file", filepath.Join(home, ".local/state/control-panel/audit.jsonl"), "audit log")
	fs.StringVar(&cfg.ciDir, "ci-dir", filepath.Join(home, "projects/ci-cd-jobs"), "GitHub runner directories")
	fs.StringVar(&cfg.statusDir, "status-dir", "/tmp/claude-tab-status", "claude-tab-status output")
	fs.BoolVar(&cfg.embedLocal, "embed-local", false, "partition session cookies for embedding via localhost (requires -insecure-no-access)")
	fs.BoolVar(&cfg.insecure, "insecure-no-access", false, "skip Cloudflare Access checks (local testing only)")
	fs.BoolVar(&cfg.behindCube, "behind-cube", false, "run as a Cube app: Cube's gate signs every visitor in, so no PIN and no Access (not with -cf-team)")

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "set-pin" {
		fs.Parse(args[1:])
		if err := setPIN(cfg.pinFile); err != nil {
			fmt.Fprintln(os.Stderr, "set-pin:", err)
			os.Exit(1)
		}
		return
	}
	fs.Parse(args)
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.behindCube {
		log.Println("-behind-cube: Cube's gate signs visitors in; the panel asks for no PIN.")
	} else if cfg.insecure {
		log.Println("WARNING: -insecure-no-access: Cloudflare Access is NOT checked. Local testing only.")
	}
	s := newServer(cfg)
	go s.historyLoop()
	go s.sampleLoop()
	go s.limits.run()
	srv := &http.Server{Addr: cfg.addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	log.Printf("control panel on http://%s", cfg.addr)
	log.Fatal(srv.ListenAndServe())
}

// setPIN asks for the PIN twice without echo and stores only its PBKDF2 hash (mode 0600).
func setPIN(path string) error {
	in := bufio.NewReader(os.Stdin)
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice != 0 {
			stty := func(arg string) { c := exec.Command("stty", arg); c.Stdin = os.Stdin; c.Run() }
			stty("-echo")
			defer func() { stty("echo"); fmt.Fprintln(os.Stderr) }()
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	pin, err := read("New PIN (6-32 digits): ")
	if err != nil {
		return err
	}
	if !validPIN(pin) {
		return errors.New("PIN must be 6-32 digits")
	}
	again, err := read("Repeat PIN: ")
	if err != nil {
		return err
	}
	if again != pin {
		return errors.New("PINs don't match")
	}
	enc, err := hashPIN(pin, pinIterations)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(enc+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "PIN saved to", path, "— it applies immediately, no restart needed.")
	return nil
}
