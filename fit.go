package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The terminal view sizes a pane's tab to the room it has. zellij sizes each tab by the clients looking at it and
// a tab keeps its size once they leave (a tab last seen on a phone stays 50 columns wide, whatever the others are),
// so a helper client of that size, attached for a moment and taken to the pane's tab, resizes that tab for good.
// A session that has a terminal attached is that terminal's: left alone.

var fitMu sync.Mutex // one resize at a time

// fit gives the tab holding pane cols×rows (pane < 0: the tab a client lands on). The helper goes back to the tab
// it landed on before it leaves, so your next attach lands where it did. false: someone's terminal is attached.
func (z *zellij) fit(session string, pane, cols, rows int) (bool, error) {
	clients, err := z.action(session, "list-clients")
	if err != nil {
		return false, err
	}
	if strings.Count(strings.TrimSpace(string(clients)), "\n") > 0 { // more than the header
		return false, nil
	}
	pos := -1 // the pane's tab
	if pane >= 0 {
		ps, err := z.panes(session)
		if err != nil {
			return false, err
		}
		for _, p := range ps {
			if p.ID == pane {
				pos = p.TabPos
			}
		}
	}
	look := func() (c, r, on int) { // the pane's tab's size (else the first tab's), and the tab a client is on (-1: none)
		out, _ := z.action(session, "list-tabs", "--json")
		var tabs []struct {
			Pos    int  `json:"position"`
			Cols   int  `json:"display_area_columns"`
			Rows   int  `json:"display_area_rows"`
			Active bool `json:"active"`
		}
		json.Unmarshal(out, &tabs)
		on = -1
		for i, t := range tabs {
			if t.Pos == pos || (pos < 0 && i == 0) {
				c, r = t.Cols, t.Rows
			}
			if t.Active {
				on = t.Pos
			}
		}
		return c, r, on
	}
	c0, r0, _ := look()
	if c0 == cols && r0 >= rows-2 && r0 <= rows { // there already (a tab bar takes a row or two)
		return true, nil
	}
	stop, err := z.attach(session, cols, rows)
	if err != nil {
		return false, err
	}
	defer stop()
	home := -1
	back := func() {
		if home >= 0 {
			z.action(session, "go-to-tab", strconv.Itoa(home+1))
		}
	}
	for i := 0; i < 40; i++ { // until zellij has resized the pane's tab
		c, r, on := look()
		if pos >= 0 && on >= 0 && on != pos && home < 0 { // the helper is in, on another tab: over to the pane's
			home = on
			z.action(session, "go-to-tab", strconv.Itoa(pos+1))
			continue
		}
		if c != c0 || r != r0 {
			back()
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	back()
	return false, errors.New("zellij didn't take the new size")
}

// paneFit: POST {session, pane, cols, rows} from the terminal view.
func (s *server) paneFit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session    string
		Pane       *int // the pane on screen: its tab is the one resized
		Cols, Rows int
	}
	if err := readJSONN(r, &b, 1<<10); err != nil || !sessionNameRe.MatchString(b.Session) || len(b.Session) > 64 ||
		b.Cols < 40 || b.Cols > 400 || b.Rows < 10 || b.Rows > 150 {
		writeJSON(w, 400, map[string]any{"error": "bad session or size"})
		return
	}
	fitMu.Lock()
	defer fitMu.Unlock()
	pane := -1
	if b.Pane != nil {
		pane = *b.Pane
	}
	ok, err := s.z.fit(b.Session, pane, b.Cols, b.Rows)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"fitted": ok})
}
