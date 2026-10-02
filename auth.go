package main

import (
	"bytes"
	"crypto"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pinIterations = 600_000
	maxFails      = 5
	lockoutFor    = 15 * time.Minute
	sessionIdle   = 12 * time.Hour
	jwtSkew       = 60 * time.Second
)

var (
	errBadPIN = errors.New("wrong PIN")
	errLocked = errors.New("locked: too many wrong PINs")
	errNoPIN  = errors.New("no PIN set: run `control-panel set-pin` on the VM")
)

// ---- PIN hashing ----

func validPIN(pin string) bool {
	if len(pin) < 6 || len(pin) > 32 {
		return false
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}
	return true
}

func hashPIN(pin string, iter int) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pin, salt, iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func verifyPIN(encoded, pin string) bool {
	p := strings.Split(strings.TrimSpace(encoded), "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pin, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// ---- audit log ----

type auditEvent struct {
	T      int64  `json:"t"`
	Event  string `json:"event"`
	Detail string `json:"detail,omitempty"`
	IP     string `json:"ip,omitempty"`
}

type auditLog struct {
	mu     sync.Mutex
	path   string
	recent []auditEvent // oldest first, capped at auditKeep
}

const auditKeep = 200

func openAudit(path string) *auditLog {
	l := &auditLog{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) { // not bufio.Scanner: one oversized line would end the scan
		var e auditEvent
		if json.Unmarshal(line, &e) == nil && e.Event != "" {
			l.recent = append(l.recent, e)
		}
	}
	if n := len(l.recent); n > auditKeep {
		l.recent = l.recent[n-auditKeep:]
	}
	return l
}

func (l *auditLog) add(event, detail, ip string) {
	e := auditEvent{T: time.Now().Unix(), Event: trunc(event, 40), Detail: trunc(detail, 256), IP: trunc(ip, 64)}
	line, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = append(l.recent, e)
	if n := len(l.recent); n > auditKeep {
		l.recent = l.recent[n-auditKeep:]
	}
	os.MkdirAll(filepath.Dir(l.path), 0o700)
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit:", err)
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}

// list returns events newest first.
func (l *auditLog) list() []auditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]auditEvent, len(l.recent))
	for i, e := range l.recent {
		out[len(out)-1-i] = e
	}
	return out
}

// ---- PIN checks, lockout, sessions ----

type session struct {
	email   string
	expires time.Time
}

type auth struct {
	pinFile string
	audit   *auditLog
	now     func() time.Time
	gated   bool // -behind-cube: Cube's gate signed the visitor in, so every request is a session

	// ponytail: one global lock and failure counter — single-user panel; per-IP
	// counters would let an attacker rotate IPs, so global is the stricter choice.
	mu          sync.Mutex
	fails       int
	lockedUntil time.Time
	failTimes   []time.Time
	sessions    map[string]session
}

func newAuth(pinFile string, audit *auditLog, now func() time.Time) *auth {
	a := &auth{pinFile: pinFile, audit: audit, now: now, sessions: map[string]session{}}
	for _, e := range audit.list() { // restore the 24 h failure history across restarts
		if e.Event == "pin-fail" {
			a.failTimes = append(a.failTimes, time.Unix(e.T, 0))
		}
	}
	return a
}

// checkPIN verifies pin against the pin file. Login and actions share the
// failure counter, and calls are serialized so guesses can't run in parallel.
func (a *auth) checkPIN(pin, ip string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if now.Before(a.lockedUntil) {
		return errLocked
	}
	enc, err := os.ReadFile(a.pinFile)
	if err != nil {
		return errNoPIN
	}
	if validPIN(pin) && verifyPIN(string(enc), pin) {
		a.fails = 0
		return nil
	}
	a.fails++
	a.failTimes = append(a.failTimes, now)
	a.audit.add("pin-fail", fmt.Sprintf("attempt %d/%d", a.fails, maxFails), ip)
	if a.fails >= maxFails {
		a.fails = 0
		a.lockedUntil = now.Add(lockoutFor)
		a.audit.add("lockout", "locked for "+lockoutFor.String(), ip)
	}
	return errBadPIN
}

func (a *auth) lockedFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return max(0, a.lockedUntil.Sub(a.now()))
}

func (a *auth) fails24h() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-24 * time.Hour)
	keep := a.failTimes[:0]
	for _, t := range a.failTimes {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.failTimes = keep
	return len(keep)
}

func (a *auth) newSession(email string) string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
	a.sessions[tok] = session{email: email, expires: now.Add(sessionIdle)}
	return tok
}

// validSession checks the token belongs to the same Access identity and slides its idle expiry.
func (a *auth) validSession(tok, email string) bool {
	if a.gated {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tok]
	if !ok || tok == "" {
		return false
	}
	now := a.now()
	if now.After(s.expires) {
		delete(a.sessions, tok)
		return false
	}
	if s.email != email {
		return false
	}
	s.expires = now.Add(sessionIdle)
	a.sessions[tok] = s
	return true
}

// peekSession checks validity without extending it: an open live stream is not user activity.
func (a *auth) peekSession(tok, email string) bool {
	if a.gated {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tok]
	return ok && tok != "" && s.email == email && !a.now().After(s.expires)
}

func (a *auth) endSession(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

// ---- Cloudflare Access JWT ----

type access struct {
	team, aud string
	fetch     func() (map[string]*rsa.PublicKey, error)
	now       func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetched   time.Time // last successful fetch
	attempted time.Time // last fetch attempt, successful or not
}

func newAccess(team, aud string) *access {
	v := &access{team: team, aud: aud, now: time.Now}
	v.fetch = func() (map[string]*rsa.PublicKey, error) { return fetchCerts(team) }
	return v
}

// key returns the signing key for kid. Keys refresh hourly; an unknown kid or a failing
// fetch triggers at most one fetch per minute (the fetch holds the lock, so it must be rare).
func (v *access) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	k, ok := v.keys[kid]
	if ok && now.Sub(v.fetched) < time.Hour {
		return k, nil
	}
	if !v.attempted.IsZero() && now.Sub(v.attempted) < time.Minute {
		if ok {
			return k, nil // stale but still the best we have
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	v.attempted = now
	keys, err := v.fetch()
	if err != nil {
		if ok { // keep serving from cache if Cloudflare is unreachable
			return k, nil
		}
		return nil, fmt.Errorf("fetch Access certs: %w", err)
	}
	v.keys, v.fetched = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

func (v *access) verify(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var hdr struct{ Alg, Kid string }
	if err := decodeSeg(parts[0], &hdr); err != nil {
		return "", err
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("alg %q not allowed", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed signature")
	}
	pub, err := v.key(hdr.Kid)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	var c struct {
		Aud   json.RawMessage
		Iss   string
		Exp   int64
		Nbf   int64
		Email string
	}
	if err := decodeSeg(parts[1], &c); err != nil {
		return "", err
	}
	var auds []string
	if json.Unmarshal(c.Aud, &auds) != nil {
		var one string
		json.Unmarshal(c.Aud, &one)
		auds = []string{one}
	}
	audOK := false
	for _, a := range auds {
		audOK = audOK || a == v.aud
	}
	now := v.now()
	switch {
	case !audOK:
		return "", errors.New("wrong aud")
	case c.Iss != "https://"+v.team+".cloudflareaccess.com":
		return "", errors.New("wrong iss")
	case c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(jwtSkew)):
		return "", errors.New("expired")
	case c.Nbf != 0 && now.Add(jwtSkew).Before(time.Unix(c.Nbf, 0)):
		return "", errors.New("not yet valid")
	case c.Email == "":
		return "", errors.New("no email in token")
	}
	return c.Email, nil
}

func decodeSeg(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("malformed token")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("malformed token")
	}
	return nil
}

func fetchCerts(team string) (map[string]*rsa.PublicKey, error) {
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://" + team + ".cloudflareaccess.com/cdn-cgi/access/certs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("certs: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseJWKS(body)
}

func parseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct{ Kid, Kty, N, E string }
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
		e, err2 := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no RSA keys in JWKS")
	}
	return keys, nil
}
