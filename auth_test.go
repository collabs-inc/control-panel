package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestAuth(t *testing.T, pin string) (*auth, *fakeClock) {
	t.Helper()
	dir := t.TempDir()
	pinFile := filepath.Join(dir, "pin")
	if pin != "" {
		enc, err := hashPIN(pin, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pinFile, []byte(enc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clk := &fakeClock{time.Unix(1_800_000_000, 0)}
	return newAuth(pinFile, openAudit(filepath.Join(dir, "audit.jsonl")), clk.now), clk
}

func TestPINRoundTrip(t *testing.T) {
	enc, err := hashPIN("482913", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPIN(enc, "482913") {
		t.Fatal("correct PIN rejected")
	}
	if verifyPIN(enc, "482914") {
		t.Fatal("wrong PIN accepted")
	}
	if verifyPIN("garbage", "482913") {
		t.Fatal("malformed hash accepted")
	}
}

func TestValidPIN(t *testing.T) {
	for pin, want := range map[string]bool{
		"123456": true, "12345678901234567890123456789012": true,
		"12345": false, "": false, "12345a": false, " 123456": false,
		"123456789012345678901234567890123": false, "١٢٣٤٥٦": false,
	} {
		if validPIN(pin) != want {
			t.Errorf("validPIN(%q) = %v, want %v", pin, !want, want)
		}
	}
}

func TestLockoutAfterFiveFailures(t *testing.T) {
	a, clk := newTestAuth(t, "482913")
	for i := 0; i < 5; i++ {
		if err := a.checkPIN("000000", "1.2.3.4"); !errors.Is(err, errBadPIN) {
			t.Fatalf("attempt %d: got %v, want errBadPIN", i, err)
		}
	}
	if err := a.checkPIN("482913", "1.2.3.4"); !errors.Is(err, errLocked) {
		t.Fatalf("correct PIN during lockout: got %v, want errLocked", err)
	}
	if d := a.lockedFor(); d <= 14*time.Minute {
		t.Fatalf("lockedFor = %v, want ~15m", d)
	}
	clk.t = clk.t.Add(15*time.Minute + time.Second)
	if err := a.checkPIN("482913", "1.2.3.4"); err != nil {
		t.Fatalf("after lockout expiry: %v", err)
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	a, _ := newTestAuth(t, "482913")
	for i := 0; i < 4; i++ {
		a.checkPIN("000000", "")
	}
	if err := a.checkPIN("482913", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		a.checkPIN("000000", "")
	}
	if err := a.checkPIN("482913", ""); err != nil {
		t.Fatalf("4 failures after a success should not lock: %v", err)
	}
}

func TestNoPINFile(t *testing.T) {
	a, _ := newTestAuth(t, "")
	if err := a.checkPIN("482913", ""); !errors.Is(err, errNoPIN) {
		t.Fatalf("got %v, want errNoPIN", err)
	}
}

func TestFailuresAreAudited(t *testing.T) {
	a, _ := newTestAuth(t, "482913")
	for i := 0; i < 5; i++ {
		a.checkPIN("000000", "9.9.9.9")
	}
	if n := a.fails24h(); n != 5 {
		t.Fatalf("fails24h = %d, want 5", n)
	}
	ev := a.audit.list()
	if len(ev) == 0 || ev[0].Event != "lockout" || ev[0].IP != "9.9.9.9" {
		t.Fatalf("newest audit event = %+v, want lockout from 9.9.9.9", ev)
	}
}

func TestAuditPersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	openAudit(p).add("login", "ok", "1.1.1.1")
	ev := openAudit(p).list()
	if len(ev) != 1 || ev[0].Event != "login" {
		t.Fatalf("reopened audit = %+v", ev)
	}
}

func TestSessionIdleExpiryAndEmailBinding(t *testing.T) {
	a, clk := newTestAuth(t, "482913")
	tok := a.newSession("me@x.com")
	if !a.validSession(tok, "me@x.com") {
		t.Fatal("fresh session invalid")
	}
	if a.validSession(tok, "other@x.com") {
		t.Fatal("session accepted for another Access identity")
	}
	clk.t = clk.t.Add(11 * time.Hour)
	if !a.validSession(tok, "me@x.com") {
		t.Fatal("session expired before 12h idle")
	}
	clk.t = clk.t.Add(11 * time.Hour) // 11h since last use: still valid (sliding)
	if !a.validSession(tok, "me@x.com") {
		t.Fatal("sliding expiry not extended")
	}
	clk.t = clk.t.Add(12*time.Hour + time.Second)
	if a.validSession(tok, "me@x.com") {
		t.Fatal("session valid after 12h idle")
	}
	tok2 := a.newSession("me@x.com")
	a.endSession(tok2)
	if a.validSession(tok2, "me@x.com") {
		t.Fatal("ended session still valid")
	}
	if a.validSession("", "me@x.com") {
		t.Fatal("empty token valid")
	}
}

// ---- Cloudflare Access JWT ----

const testTeam = "myteam"
const testAUD = "aud-tag-123"

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func newTestAccess(t *testing.T) (*access, *rsa.PrivateKey, *fakeClock) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{time.Unix(1_800_000_000, 0)}
	v := newAccess(testTeam, testAUD)
	v.now = clk.now
	v.fetch = func() (map[string]*rsa.PublicKey, error) {
		return map[string]*rsa.PublicKey{"k1": &key.PublicKey}, nil
	}
	return v, key, clk
}

func goodClaims(clk *fakeClock) map[string]any {
	return map[string]any{
		"aud":   []string{testAUD},
		"iss":   "https://" + testTeam + ".cloudflareaccess.com",
		"exp":   clk.t.Add(time.Hour).Unix(),
		"iat":   clk.t.Unix(),
		"nbf":   clk.t.Unix(),
		"email": "me@x.com",
	}
}

var goodHeader = map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}

func TestAccessAcceptsValidToken(t *testing.T) {
	v, key, clk := newTestAccess(t)
	email, err := v.verify(signJWT(t, key, goodHeader, goodClaims(clk)))
	if err != nil || email != "me@x.com" {
		t.Fatalf("verify = %q, %v", email, err)
	}
	c := goodClaims(clk)
	c["aud"] = testAUD // Cloudflare may send a bare string
	if _, err := v.verify(signJWT(t, key, goodHeader, c)); err != nil {
		t.Fatalf("string aud rejected: %v", err)
	}
}

func TestAccessRejects(t *testing.T) {
	v, key, clk := newTestAccess(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	mod := func(f func(c map[string]any)) map[string]any { c := goodClaims(clk); f(c); return c }
	cases := map[string]string{
		"bad signature": signJWT(t, other, goodHeader, goodClaims(clk)),
		"wrong aud":     signJWT(t, key, goodHeader, mod(func(c map[string]any) { c["aud"] = []string{"nope"} })),
		"wrong iss":     signJWT(t, key, goodHeader, mod(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })),
		"expired":       signJWT(t, key, goodHeader, mod(func(c map[string]any) { c["exp"] = clk.t.Add(-2 * time.Minute).Unix() })),
		"not yet valid": signJWT(t, key, goodHeader, mod(func(c map[string]any) { c["nbf"] = clk.t.Add(5 * time.Minute).Unix() })),
		"missing exp":   signJWT(t, key, goodHeader, mod(func(c map[string]any) { delete(c, "exp") })),
		"unknown kid":   signJWT(t, key, map[string]any{"alg": "RS256", "kid": "k9"}, goodClaims(clk)),
		"alg none":      signJWT(t, key, map[string]any{"alg": "none", "kid": "k1"}, goodClaims(clk)),
		"alg HS256":     signJWT(t, key, map[string]any{"alg": "HS256", "kid": "k1"}, goodClaims(clk)),
		"malformed":     "a.b",
		"empty":         "",
	}
	for name, tok := range cases {
		if _, err := v.verify(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAccessRefetchesOnUnknownKidAtMostOncePerMinute(t *testing.T) {
	v, key, clk := newTestAccess(t)
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	fetches := 0
	rotated := false
	v.fetch = func() (map[string]*rsa.PublicKey, error) {
		fetches++
		if rotated {
			return map[string]*rsa.PublicKey{"k2": &newKey.PublicKey}, nil
		}
		return map[string]*rsa.PublicKey{"k1": &key.PublicKey}, nil
	}
	if _, err := v.verify(signJWT(t, key, goodHeader, goodClaims(clk))); err != nil {
		t.Fatal(err)
	}
	rotated = true
	clk.t = clk.t.Add(2 * time.Minute)
	h2 := map[string]any{"alg": "RS256", "kid": "k2"}
	if _, err := v.verify(signJWT(t, newKey, h2, goodClaims(clk))); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
	before := fetches
	for i := 0; i < 5; i++ {
		v.verify(signJWT(t, newKey, map[string]any{"alg": "RS256", "kid": "zzz"}, goodClaims(clk)))
	}
	if fetches != before {
		t.Fatalf("unknown kid refetched %d times within a minute", fetches-before)
	}
}

func TestParseJWKS(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	e := []byte{1, 0, 1}
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"kid": "a", "kty": "RSA", "alg": "RS256", "n": b64(key.N.Bytes()), "e": b64(e)},
		{"kid": "b", "kty": "EC"},
	}})
	keys, err := parseJWKS(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys["a"].N.Cmp(key.N) != 0 || keys["a"].E != 65537 {
		t.Fatalf("parseJWKS = %+v", keys)
	}
}

func TestPeekSessionDoesNotSlide(t *testing.T) {
	a, clk := newTestAuth(t, "482913")
	tok := a.newSession("me@x.com")
	clk.t = clk.t.Add(11 * time.Hour)
	if !a.peekSession(tok, "me@x.com") || a.peekSession(tok, "x@y.z") {
		t.Fatal("peek wrong while valid")
	}
	clk.t = clk.t.Add(2 * time.Hour) // 13 h since the last *sliding* use
	if a.peekSession(tok, "me@x.com") {
		t.Fatal("peek extended the session: an open stream would keep it alive forever")
	}
}

func TestFailCountSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	enc, _ := hashPIN("482913", 1000)
	os.WriteFile(filepath.Join(dir, "pin"), []byte(enc), 0o600)
	auditPath := filepath.Join(dir, "audit.jsonl")
	a := newAuth(filepath.Join(dir, "pin"), openAudit(auditPath), time.Now)
	a.checkPIN("000000", "")
	a.checkPIN("000001", "")
	b := newAuth(filepath.Join(dir, "pin"), openAudit(auditPath), time.Now)
	if n := b.fails24h(); n != 2 {
		t.Fatalf("after restart fails24h = %d, want 2", n)
	}
}

func TestAuditTruncatesAndSurvivesHugeLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := openAudit(p)
	l.add("access-reject", strings.Repeat("A", 900_000), strings.Repeat("9", 5000))
	ev := l.list()
	if len(ev[0].Detail) > 256 || len(ev[0].IP) > 64 {
		t.Fatalf("stored detail=%d ip=%d bytes", len(ev[0].Detail), len(ev[0].IP))
	}
	// a pre-existing oversized line must not hide the events after it
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"t":1,"event":"junk","detail":"` + strings.Repeat("B", 200_000) + "\"}\n")
	f.Close()
	openAudit(p).add("pin-fail", "attempt 1/5", "1.2.3.4")
	got := openAudit(p).list()
	if len(got) < 2 || got[0].Event != "pin-fail" {
		t.Fatalf("after oversized line: %+v", got[:min(3, len(got))])
	}
}

func TestAccessFetchRateLimitedWhenCertsUnavailable(t *testing.T) {
	v, key, clk := newTestAccess(t)
	calls := 0
	v.fetch = func() (map[string]*rsa.PublicKey, error) { calls++; return nil, errors.New("down") }
	tok := signJWT(t, key, goodHeader, goodClaims(clk))
	for i := 0; i < 5; i++ {
		if _, err := v.verify(tok); err == nil {
			t.Fatal("verified without keys")
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times within a minute while certs unavailable", calls)
	}
}

func TestSessionsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	pin := filepath.Join(dir, "pin")
	audit := openAudit(filepath.Join(dir, "audit.jsonl"))
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	a := newAuth(pin, audit, clock)
	tok := a.newSession("me@x")
	gone := a.newSession("me@x")
	a.endSession(gone) // logged out: stays out

	b := newAuth(pin, audit, clock) // the panel restarted
	if !b.validSession(tok, "me@x") {
		t.Fatal("a session must survive a restart")
	}
	if b.validSession(gone, "me@x") || b.validSession(tok, "someone@else") {
		t.Fatal("a logged-out session, or another identity, must not")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if strings.Contains(string(raw), tok) || len(raw) == 0 {
		t.Fatalf("the file keeps only a hash of each token: %s", raw)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "sessions.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json mode %v", fi.Mode().Perm())
	}
	now = now.Add(sessionIdle + time.Minute)
	if c := newAuth(pin, audit, clock); c.validSession(tok, "me@x") {
		t.Fatal("an expired session must not come back")
	}
}
