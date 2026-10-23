package main

import (
	"fmt"
	"testing"
)

func TestZellijFit(t *testing.T) {
	tabs := func(c, r int) string {
		return fmt.Sprintf(`[{"display_area_columns":%d,"display_area_rows":%d}]`, c, r)
	}
	z, f := newFakeZellij(map[string]string{
		"--session dev-1 action list-clients": "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n",
		"--session dev-1 action list-tabs":    tabs(48, 47),
	})
	f.onCall = func(args []string) { // zellij takes the helper client's size once it's attached
		if len(f.attached) > 0 {
			f.answers["--session dev-1 action list-tabs"] = tabs(180, 50)
		}
	}
	if ok, err := z.fit("dev-1", -1, 180, 50); !ok || err != nil || fmt.Sprint(f.attached) != "[dev-1 180x50 detach]" {
		t.Fatalf("detached session: %v %v %v", ok, err, f.attached)
	}
	f.attached = nil
	if ok, _ := z.fit("dev-1", -1, 180, 50); !ok || len(f.attached) != 0 {
		t.Fatalf("already that size: nothing to attach, got %v", f.attached)
	}
	f.answers["--session dev-1 action list-clients"] = "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_0 zsh\n"
	if ok, err := z.fit("dev-1", -1, 120, 40); ok || err != nil || len(f.attached) != 0 {
		t.Fatalf("someone's terminal is attached: leave it, got %v %v %v", ok, err, f.attached)
	}
}

// zellij sizes each tab by the clients looking at it, and a tab keeps its size when they leave: a tab last seen on
// a phone stays 50 columns wide whatever size the session's first tab has. The helper goes to the pane's tab.
func TestZellijFitResizesThePanesOwnTab(t *testing.T) {
	z, f := newFakeZellij(map[string]string{
		"--session box-dev action list-clients": "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n",
		"--session box-dev action list-panes":   `[{"id":0,"tab_id":0,"tab_position":0},{"id":1,"tab_id":1,"tab_position":1}]`,
		"--session box-dev action go-to-tab":    "",
	})
	size := [][2]int{{229, 69}, {50, 50}}
	on := -1 // the tab the helper client is on
	var moves []string
	f.onCall = func(args []string) {
		if on < 0 && len(f.attached) == 1 {
			on, size[0] = 0, [2]int{229, 69} // it lands on the first tab
		}
		if len(args) > 3 && args[3] == "go-to-tab" {
			fmt.Sscan(args[4], &on)
			on--
			size[on] = [2]int{229, 69}
			moves = append(moves, args[4])
		}
		js := "["
		for i, s := range size {
			js += fmt.Sprintf(`%s{"position":%d,"display_area_columns":%d,"display_area_rows":%d,"active":%v}`, map[bool]string{true: ",", false: ""}[i > 0], i, s[0], s[1], i == on)
		}
		f.answers["--session box-dev action list-tabs"] = js + "]"
	}
	if ok, err := z.fit("box-dev", 1, 229, 69); !ok || err != nil {
		t.Fatalf("fit: %v %v", ok, err)
	}
	if size[1] != [2]int{229, 69} || fmt.Sprint(moves) != "[2 1]" || fmt.Sprint(f.attached) != "[box-dev 229x69 detach]" {
		t.Fatalf("the pane's tab: %v, moves %v (to it, then back), attached %v", size[1], moves, f.attached)
	}
	f.attached, moves = nil, nil
	if ok, _ := z.fit("box-dev", 1, 229, 69); !ok || len(f.attached) != 0 {
		t.Fatalf("its tab has the size now: nothing to do, got %v", f.attached)
	}
}
