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
	fs.BoolVar(&cfg.insecure, "insecure-no-access", false, "skip Cloudflare Access checks (local testing only)")
	fs.StringVar(&cfg.agentAddr, "agent-addr", "127.0.0.1:7778", "loopback API for the owner's assistant (vm-agent); empty turns it off")
	fs.StringVar(&cfg.aiURL, "ai-url", "http://127.0.0.1:8642", "the assistant's Hermes API server (loopback)")
	fs.StringVar(&cfg.hermesEnv, "hermes-env", filepath.Join(home, ".hermes/.env"), "Hermes' .env, where Connections saves the Telegram settings")
	fs.StringVar(&cfg.aiKeyFile, "ai-key-file", filepath.Join(home, ".config/vm-agent/api-key"), "its API key, written by vm-agent/setup.sh")
	fs.StringVar(&cfg.compactScope, "compact-sessions", "", "the zellij sessions whose agents the compactor may type into (regexp; empty: all)")
	fs.StringVar(&cfg.agentTokenFile, "agent-token-file", filepath.Join(home, ".config/control-panel/agent-token"), "bearer token for -agent-addr (created, mode 0600, if missing)")

	fs.BoolVar(&cfg.behindCube, "behind-cube", false, "trust Cube app gate authentication")
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
	if cfg.behindCube {
		cfg.insecure = true
	}
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.insecure {
		log.Println("WARNING: -insecure-no-access: Cloudflare Access is NOT checked. Local testing only.")
	}
	s := newServer(cfg)
	go s.historyLoop()
	go s.sampleLoop()
	go s.tokensLoop()
	go s.limits.run()
	go func() { // agents' browsers become watchable (browseragents.go); the panel works without it
		if err := s.br.installAgentWatch(); err != nil {
			log.Println("browser: agent watch:", err)
		}
		s.br.agentsScan() // from the start: the trails record whether or not anyone has the Browser open
	}()
	if cfg.agentAddr != "" {
		tok, err := agentToken(cfg.agentTokenFile)
		if err != nil {
			log.Fatal("agent token: ", err)
		}
		agent := &http.Server{Addr: cfg.agentAddr, Handler: s.agentHandler(tok), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
		go func() { log.Println("agent API:", agent.ListenAndServe()) }() // the page keeps working without it
		log.Printf("agent API on http://%s", cfg.agentAddr)
	}
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
	if err := writeFileAtomic(path, []byte(enc+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "PIN saved to", path, "— it applies immediately, no restart needed.")
	return nil
}
