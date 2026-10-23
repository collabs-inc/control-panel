package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A websocket (RFC 6455), as little of it as the Browser needs: the page's view of a tab (frames down, input up)
// and the agents' Chromiums, which speak CDP on a localhost websocket. The standard library has neither end, and the
// panel takes no dependencies. No extensions (frames are JPEG already), no subprotocols.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsText   = 1
	wsBinary = 2
	wsClose  = 8
	wsPing   = 9
	wsPong   = 10
)

var errWSTooBig = errors.New("websocket message too big")

type wsConn struct {
	c      net.Conn
	r      *bufio.Reader
	wmu    sync.Mutex
	client bool  // a client masks what it sends
	max    int64 // the largest message read accepts
	closed bool
}

func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsUpgrade answers a websocket handshake and takes the connection over. Only a same-origin page may open one:
// a socket isn't covered by the cross-origin check the panel's POSTs get, and the cookie alone isn't enough.
func wsUpgrade(w http.ResponseWriter, r *http.Request, max int64) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHas(r.Header, "Connection", "upgrade") {
		http.Error(w, "websocket expected", http.StatusBadRequest)
		return nil, errors.New("not a websocket handshake")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket version 13 only", http.StatusUpgradeRequired)
		return nil, errors.New("websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "no websocket key", http.StatusBadRequest)
		return nil, errors.New("no key")
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "cross-origin websocket", http.StatusForbidden)
			return nil, errors.New("cross-origin websocket from " + trunc(o, 80))
		}
	}
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "can't take the connection over", 500)
		return nil, err
	}
	c.SetDeadline(time.Time{})
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAcceptKey(key))
	if err := rw.Flush(); err != nil {
		c.Close()
		return nil, err
	}
	return &wsConn{c: c, r: rw.Reader, max: max}, nil
}

func headerHas(h http.Header, k, token string) bool {
	for _, v := range h.Values(k) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wsDial opens a ws:// URL (Chromium's DevTools endpoint on localhost: never TLS).
func wsDial(raw string, max int64) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ws" {
		return nil, fmt.Errorf("bad websocket URL %q", raw)
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var k [16]byte
	rand.Read(k[:])
	key := base64.StdEncoding.EncodeToString(k[:])
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, key)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(key) {
		c.Close()
		return nil, fmt.Errorf("websocket handshake: %s", resp.Status)
	}
	c.SetDeadline(time.Time{})
	return &wsConn{c: c, r: br, client: true, max: max}, nil
}

// read returns the next data message (text or binary), whole: fragments are joined, a ping is answered, and a
// close (or a broken connection) is io.EOF.
func (ws *wsConn) read() (int, []byte, error) {
	var msg []byte
	op := 0
	for {
		var h [2]byte
		if _, err := io.ReadFull(ws.r, h[:]); err != nil {
			return 0, nil, err
		}
		fin, code, masked := h[0]&0x80 != 0, int(h[0]&0x0F), h[1]&0x80 != 0
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(ws.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(ws.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint64(b[:]) & (1<<63 - 1))
		}
		if n > ws.max || int64(len(msg))+n > ws.max {
			ws.closeWith(1009)
			return 0, nil, errWSTooBig
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(ws.r, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(ws.r, p); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= mask[i&3]
			}
		}
		switch code {
		case wsPing:
			ws.write(wsPong, p)
			continue
		case wsPong:
			continue
		case wsClose:
			ws.closeWith(1000)
			return 0, nil, io.EOF
		case 0: // a continuation
			if op == 0 {
				return 0, nil, errors.New("websocket continuation without a start")
			}
		default:
			if op != 0 {
				return 0, nil, errors.New("websocket message inside another")
			}
			op = code
		}
		msg = append(msg, p...)
		if fin {
			return op, msg, nil
		}
	}
}

// write sends one whole message; safe from several goroutines.
func (ws *wsConn) write(op int, p []byte) error {
	ws.wmu.Lock()
	defer ws.wmu.Unlock()
	if ws.closed {
		return net.ErrClosed
	}
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|byte(op))
	mbit := byte(0)
	if ws.client {
		mbit = 0x80
	}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, mbit|byte(n))
	case n < 1<<16:
		hdr = append(hdr, mbit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := p
	if ws.client {
		var mask [4]byte
		rand.Read(mask[:])
		hdr = append(hdr, mask[:]...)
		body = make([]byte, len(p))
		for i := range p {
			body[i] = p[i] ^ mask[i&3]
		}
	}
	ws.c.SetWriteDeadline(time.Now().Add(20 * time.Second)) // a peer that stopped reading can't hold the writer forever
	if _, err := (&net.Buffers{hdr, body}).WriteTo(ws.c); err != nil {
		ws.closed = true
		ws.c.Close()
		return err
	}
	return nil
}

func (ws *wsConn) closeWith(code int) {
	ws.write(wsClose, []byte{byte(code >> 8), byte(code)})
	ws.close()
}

func (ws *wsConn) close() {
	ws.wmu.Lock()
	ws.closed = true
	ws.wmu.Unlock()
	ws.c.Close()
}
