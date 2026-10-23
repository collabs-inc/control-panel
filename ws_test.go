package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// an echo server: what the client sends comes back with the same opcode
func wsEcho(t *testing.T, max int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsUpgrade(w, r, max)
		if err != nil {
			return
		}
		defer ws.close()
		for {
			op, p, err := ws.read()
			if err != nil {
				return
			}
			ws.write(op, p)
		}
	}))
}

func TestWSRoundTrip(t *testing.T) {
	srv := wsEcho(t, 1<<20)
	defer srv.Close()
	ws, err := wsDial("ws"+strings.TrimPrefix(srv.URL, "http")+"/x", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.close()
	for _, n := range []int{0, 5, 125, 126, 127, 65535, 65536, 300000} { // each length encoding, both sides of it
		p := bytes.Repeat([]byte{byte(n)}, n)
		op := wsBinary
		if n%2 == 1 {
			op = wsText
		}
		if err := ws.write(op, p); err != nil {
			t.Fatal(err)
		}
		gop, got, err := ws.read()
		if err != nil || gop != op || !bytes.Equal(got, p) {
			t.Fatalf("%d bytes: op %d len %d err %v", n, gop, len(got), err)
		}
	}
}

// fragments are joined; a ping between them is answered and doesn't end the message
func TestWSFragments(t *testing.T) {
	srv := wsEcho(t, 1<<20)
	defer srv.Close()
	ws, err := wsDial("ws"+strings.TrimPrefix(srv.URL, "http")+"/", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.close()
	frame := func(b0 byte, p string) []byte { // masked with a zero mask: the bytes go as they are
		return append([]byte{b0, 0x80 | byte(len(p)), 0, 0, 0, 0}, p...)
	}
	raw := append(frame(wsText, "hel"), frame(0x80|wsPing, "pp")...)
	raw = append(raw, frame(0x80, "lo")...)
	if _, err := ws.c.Write(raw); err != nil {
		t.Fatal(err)
	}
	op, p, err := ws.read() // the pong comes back first and is skipped
	if err != nil || op != wsText || string(p) != "hello" {
		t.Fatalf("got %d %q %v", op, p, err)
	}
}

func TestWSLimitsAndOrigin(t *testing.T) {
	srv := wsEcho(t, 1000)
	defer srv.Close()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ws, err := wsDial(u, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ws.write(wsBinary, make([]byte, 2000)) // past the server's limit: it closes
	if _, _, err := ws.read(); err != io.EOF {
		t.Fatalf("an oversized message: %v, want the server to close", err)
	}
	ws.close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	for k, v := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Origin": "https://evil.example"} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin handshake got %d, want 403", resp.StatusCode)
	}
	if k := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); k != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" { // RFC 6455's own example
		t.Fatalf("accept key %s", k)
	}
}
