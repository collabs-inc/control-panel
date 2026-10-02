package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDuStreamKeepsFoldersOfAMegabyteAndUp(t *testing.T) {
	out := "4096\t/h/small\n3145728\t/h/a/x\n5242880\t/h/a\n9000000\t/h\n2097152\t/elsewhere\n"
	m := parseDuStream(strings.NewReader(out), "/h")
	if len(m) != 3 || m["/h"] != 9000000 || m["/h/a"] != 5242880 || m["/h/a/x"] != 3145728 {
		t.Fatalf("index = %v", m)
	}
}

// a home with a big folder, a small one, a file and a symlink, and the index du would have built
func diskFixture(t *testing.T) (*duIndex, string) {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "big/inner"), 0o755)
	os.MkdirAll(filepath.Join(root, "tiny"), 0o755)
	os.WriteFile(filepath.Join(root, "big/inner/blob"), make([]byte, 3<<20), 0o644)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi"), 0o644)
	os.Symlink("/etc", filepath.Join(root, "link"))
	x := &duIndex{root: root, file: filepath.Join(t.TempDir(), "du.json"), deleting: map[string]bool{}}
	x.setLocked(map[string]int64{root: 5 << 20, filepath.Join(root, "big"): 4 << 20, filepath.Join(root, "big/inner"): 3 << 20}, 1000)
	return x, root
}

func TestDiskViewListsFoldersFromTheIndexAndFilesLive(t *testing.T) {
	x, root := diskFixture(t)
	v, err := x.view(root)
	if err != nil {
		t.Fatal(err)
	}
	if v.Bytes != 5<<20 || len(v.Items) != 4 {
		t.Fatalf("view = %+v", v)
	}
	big := v.Items[0]
	if big.Name != "big" || !big.Dir || big.Bytes != 4<<20 || len(big.Kids) != 1 || big.Kids[0].Name != "inner" {
		t.Fatalf("first item = %+v", big)
	}
	byName := map[string]DiskItem{}
	for _, it := range v.Items {
		byName[it.Name] = it
	}
	if byName["tiny"].Small || byName["tiny"].Bytes == 0 || !byName["link"].Link || byName["notes.txt"].Dir || byName["notes.txt"].Bytes == 0 {
		t.Fatalf("items = %+v (an unindexed folder is measured on the spot)", v.Items)
	}
	if _, err := x.view(filepath.Dir(root)); err == nil {
		t.Fatal("a folder outside the root must not be viewable")
	}
	if v, err := x.view(filepath.Join(root, "tiny")); err != nil || !v.Live {
		t.Fatalf("a folder the index doesn't hold is measured live: %+v %v", v, err)
	}
}

func TestDiskIndexPersists(t *testing.T) {
	x, root := diskFixture(t)
	x.save()
	y := &duIndex{root: root, file: x.file, deleting: map[string]bool{}}
	y.load()
	if y.at != 1000 || y.size[filepath.Join(root, "big/inner")] != 3<<20 || len(y.kids[root]) != 1 {
		t.Fatalf("loaded = at %d size %v kids %v", y.at, y.size, y.kids)
	}
	z := &duIndex{root: "/other", file: x.file, deleting: map[string]bool{}}
	z.load()
	if z.size != nil {
		t.Fatal("an index of another root must not be loaded")
	}
}

func TestCheckDeleteGuards(t *testing.T) {
	x, root := diskFixture(t)
	ci := filepath.Join(root, "ci")
	os.MkdirAll(filepath.Join(ci, "r1/_work/repo"), 0o755)
	os.MkdirAll(filepath.Join(ci, "r1/bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "via"))
	os.WriteFile(filepath.Join(root, "real/f"), nil, 0o644)
	g := deleteGuard{protect: []string{filepath.Join(root, "big/inner")}, ciDir: ci}
	for _, c := range []struct {
		path string
		ok   bool
	}{
		{root, false},                                  // home itself
		{filepath.Dir(root), false},                    // outside
		{root + "/big/../notes.txt", false},            // not clean
		{filepath.Join(root, "big"), false},            // holds a protected path
		{filepath.Join(root, "big/inner/blob"), false}, // inside one
		{filepath.Join(root, "via/f"), false},          // through a symlinked folder
		{filepath.Join(root, "missing"), false},        // nothing there
		{filepath.Join(ci, "r1/bin"), false},           // a runner's own files
		{filepath.Join(ci, "r1"), false},               // a whole runner
		{filepath.Join(ci, "r1/_work/repo"), true},     // its build cache
		{filepath.Join(root, "notes.txt"), true},       // a file
		{filepath.Join(root, "link"), true},            // a symlink (only the link goes)
		{filepath.Join(root, "tiny"), true},            // a folder
	} {
		err := x.checkDelete(c.path, g)
		if (err == nil) != c.ok {
			t.Errorf("checkDelete(%s) = %v, want ok=%v", c.path, err, c.ok)
		}
	}
}

func TestRemoveUpdatesTheIndex(t *testing.T) {
	x, root := diskFixture(t)
	done := make(chan int64, 1)
	if err := x.remove(filepath.Join(root, "big/inner"), func(freed int64, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- freed
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case freed := <-done:
		if freed != 3<<20 {
			t.Fatalf("freed = %d", freed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remove never finished")
	}
	if _, err := os.Stat(filepath.Join(root, "big/inner")); !os.IsNotExist(err) {
		t.Fatal("folder still there")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.size[filepath.Join(root, "big/inner")]; ok || x.size[filepath.Join(root, "big")] != 1<<20 || x.size[root] != 2<<20 || len(x.kids[filepath.Join(root, "big")]) != 0 {
		t.Fatalf("index after remove: %v kids %v", x.size, x.kids)
	}
}

func TestRemoveDuringAScanForgetsWhatTheScanSaw(t *testing.T) {
	x, root := diskFixture(t)
	big := filepath.Join(root, "big")
	x.running = true
	x.forget(big, 4<<20)
	x.finishScan(map[string]int64{root: 5 << 20, big: 4 << 20, filepath.Join(big, "inner"): 3 << 20}, 2000)
	if _, ok := x.size[big]; ok || x.size[root] != 1<<20 {
		t.Fatalf("index = %v", x.size)
	}
}
