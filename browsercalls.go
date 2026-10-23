package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// API calls: the XHR and fetch requests an agent's pages make, kept in its trail with what was sent and what came
// back, so the replay shows them and another agent can read a response instead of asking that API again
// (cube-browser api-calls, in the wrapper). One file per call, trails/<launch>/<run>/calls/<n>.json; an event
// ("call") in the trail's events; a line in trails/calls.jsonl, which the wrapper searches. Credentials never go to
// disk: the auth and cookie headers, and the password, secret and token fields of JSON and form bodies, are
// replaced by "[redacted]".

const (
	brCallBodyMax = 2 << 20   // one body kept (the rest cut, and said so)
	brCallsMax    = 100 << 20 // one run's calls
	brCallsN      = 3000
)

var (
	brSecretHeader = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api-key|apikey|x-auth-token|x-access-token|x-csrf-token|x-xsrf-token|x-amz-security-token)$`)
	// ponytail: by name only (a pagination "next_token" goes too); a secret under another name is kept
	brSecretKey = regexp.MustCompile(`(?i)(password|passwd|secret|api_?key|token|credentials?)$|^(pass|pin|otp)$`)
)

const brRedacted = "[redacted]"

func brRedactHeaders(h map[string]any) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		s, _ := v.(string)
		if brSecretHeader.MatchString(k) {
			s = brRedacted
		}
		out[k] = trunc(s, 2000)
	}
	return out
}

// brRedactBody: a JSON or form body with its secret fields replaced; anything else as it is
func brRedactBody(body, ctype string) string {
	if body == "" {
		return body
	}
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		if brRedactJSON(v) {
			raw, _ := json.Marshal(v)
			return string(raw)
		}
		return body
	}
	if strings.Contains(ctype, "x-www-form-urlencoded") || (!strings.ContainsAny(body, " {<\n") && strings.Contains(body, "=")) {
		if q, err := url.ParseQuery(body); err == nil {
			hit := false
			for k := range q {
				if brSecretKey.MatchString(k) {
					q[k], hit = []string{brRedacted}, true
				}
			}
			if hit {
				return q.Encode()
			}
		}
	}
	return body
}

func brRedactJSON(v any) bool {
	hit := false
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if _, str := e.(string); str && brSecretKey.MatchString(k) {
				x[k], hit = brRedacted, true
			} else if brRedactJSON(e) {
				hit = true
			}
		}
	case []any:
		for _, e := range x {
			if brRedactJSON(e) {
				hit = true
			}
		}
	}
	return hit
}

// trailCall: an API call of an agent's page has finished (or failed): its bodies, asked of Chromium while it still
// has them, and the call into the trail. On its own goroutine.
func (b *browser) trailCall(t *btab, n bnet) {
	tr := t.src.agent.trail
	if tr == nil {
		return
	}
	b.mu.Lock()
	page := t.URL
	b.mu.Unlock()
	req := map[string]any{"headers": n.reqH}
	post := n.post
	if n.hasPost && post == "" {
		var r struct{ PostData string }
		if t.src.cdp.callT(5*time.Second, t.sess, "Network.getRequestPostData", map[string]any{"requestId": n.ID}, &r) == nil {
			post = r.PostData
		}
	}
	if post != "" {
		req["body"] = brCallBody(brRedactBody(post, n.reqH["Content-Type"]+n.reqH["content-type"]), req)
	}
	res := map[string]any{"status": n.Status, "headers": n.resH}
	if n.Err == "" {
		var r struct {
			Body          string
			Base64Encoded bool
		}
		if t.src.cdp.callT(10*time.Second, t.sess, "Network.getResponseBody", map[string]any{"requestId": n.ID}, &r) == nil {
			body := r.Body
			if r.Base64Encoded {
				if raw, err := base64.StdEncoding.DecodeString(body); err == nil && utf8.Valid(raw) {
					body = string(raw) // text after all
				} else {
					res["base64"] = true // an image, a file: as Chromium gave it
				}
			}
			if res["base64"] == nil {
				body = brRedactBody(body, n.Mime)
			}
			res["body"] = brCallBody(body, res)
		}
	}
	rec := map[string]any{"t": n.T, "tab": t.id, "page": trunc(page, 2000), "method": n.Method, "url": n.URL, "type": n.Type,
		"status": n.Status, "mime": n.Mime, "ms": n.MS, "size": n.Size, "request": req, "response": res}
	if n.Err != "" {
		rec["error"] = n.Err
	}
	tr.call(rec)
}

// brCallBody: a body as kept, at most brCallBodyMax (the part said so, in its section)
func brCallBody(s string, in map[string]any) string {
	if len(s) > brCallBodyMax {
		in["truncated"] = len(s)
		s = s[:brCallBodyMax]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// call keeps an API call: its file, its event in the trail, its line in calls.jsonl.
func (tr *btrail) call(rec map[string]any) {
	raw, _ := json.MarshalIndent(rec, "", " ")
	tr.mu.Lock()
	if tr.done || tr.meta.Calls >= brCallsN || tr.callBytes+int64(len(raw)) > brCallsMax {
		tr.mu.Unlock()
		return
	}
	n := tr.meta.Calls + 1
	os.MkdirAll(filepath.Join(tr.dir, "calls"), 0o700)
	if os.WriteFile(filepath.Join(tr.dir, "calls", strconv.Itoa(n)+".json"), raw, 0o600) != nil {
		tr.mu.Unlock()
		return
	}
	tr.meta.Calls, tr.callBytes, tr.dirty = n, tr.callBytes+int64(len(raw)), true
	tr.mu.Unlock()
	body, _ := rec["response"].(map[string]any)["body"].(string)
	ev := map[string]any{"t": rec["t"], "tab": rec["tab"], "n": n, "method": rec["method"], "url": rec["url"], "status": rec["status"], "mime": rec["mime"], "ms": rec["ms"], "size": len(body)}
	if e, ok := rec["error"]; ok {
		ev["err"] = e
	}
	tr.add("call", ev)
	ev["trail"], ev["page"] = tr.id, trunc(rec["page"].(string), 300) // the page that called: its API may be on another port
	delete(ev, "k")
	line, _ := json.Marshal(ev)
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(tr.vis), "calls.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
}

// trailCallGet: one API call of a trail, for the replay
func (b *browser) trailCallGet(w http.ResponseWriter, r *http.Request) {
	dir, ok := b.trailDir(r.URL.Query().Get("id"))
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if !ok || err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, "calls", strconv.Itoa(n)+".json"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}
