package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fsEnv is a test server whose Files roots are its temp home and a second temp folder (standing in for /tmp).
type fsEnv struct {
	*testEnv
	home, other, tok string
}

func newFsEnv(t *testing.T) *fsEnv {
	e := newTestServer(t)
	f := &fsEnv{testEnv: e, home: e.s.cfg.home, other: t.TempDir(), tok: e.login(t)}
	e.s.fsx = newFsIndex(f.home, f.other)
	f.home, f.other = e.s.fsx.roots[0], e.s.fsx.roots[1]
	st := t.TempDir() // the panel's own state lives outside home, as for real (~/.config, ~/.local/state): the delete guard protects it
	e.s.cfg.pinFile, e.s.cfg.auditFile = filepath.Join(st, "pin"), filepath.Join(st, "audit.jsonl")
	return f
}

func (f *fsEnv) call(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	if s, ok := body.(string); ok {
		b = []byte(s)
	}
	r := f.do(t, method, path, string(b), f.authed(f.tok))
	if out != nil {
		json.NewDecoder(r.Body).Decode(out)
	}
	return r.StatusCode
}

func q(p string) string { return url.QueryEscape(p) }

func TestFsResolveConfinesToTheRoots(t *testing.T) {
	home, other, outside := t.TempDir(), t.TempDir(), t.TempDir()
	roots := newFsIndex(home, other).roots
	home, other = roots[0], roots[1]
	os.Mkdir(filepath.Join(home, "d"), 0o755)
	os.Symlink(outside, filepath.Join(home, "out"))
	os.Symlink(filepath.Join(other), filepath.Join(home, "in"))
	ok := map[string]string{
		"~/d":                 home + "/d",
		"~":                   home,
		home + "/d/../d/new":  home + "/d/new", // not there yet: its folder is resolved
		home + "/in/x":        other + "/x",
		other:                 other,
		home + "/out (later)": home + "/out (later)",
	}
	for in, want := range ok {
		if got, err := fsResolve(roots, in, true); err != nil || got != want {
			t.Errorf("%s -> %q %v, want %s", in, got, err, want)
		}
	}
	for _, in := range []string{home + "/../x", "~/../../etc/passwd", home + "/out", home + "/out/f", "relative/path", "", "/etc"} {
		if got, err := fsResolve(roots, in, true); err == nil {
			t.Errorf("%s resolved to %s: it is outside the roots", in, got)
		}
	}
	if got, err := fsResolve(roots, home+"/out", false); err != nil || got != home+"/out" {
		t.Errorf("without follow a link is itself (deletable), not its target: %q %v", got, err)
	}
}

func TestFsListReadsBranchesAndLinks(t *testing.T) {
	f := newFsEnv(t)
	writeFile(t, filepath.Join(f.home, "repo/.git/HEAD"), "ref: refs/heads/feat/x\n")
	writeFile(t, filepath.Join(f.home, "detached/.git/HEAD"), "0123456789abcdef\n")
	writeFile(t, filepath.Join(f.home, "repo/a.txt"), "hello")
	os.Symlink(filepath.Join(f.home, "repo"), filepath.Join(f.home, "repo-link"))
	var v struct {
		Path    string
		Entries []fsEntry
	}
	if c := f.call(t, "GET", "/api/fs/list?path=~", nil, &v); c != 200 || v.Path != f.home {
		t.Fatalf("list ~: %d %+v", c, v)
	}
	by := map[string]fsEntry{}
	for _, e := range v.Entries {
		by[e.Name] = e
	}
	if by["repo"].Br != "feat/x" || by["detached"].Br != "0123456" || !by["repo-link"].Dir || !by["repo-link"].Link || by["repo-link"].Br != "feat/x" {
		t.Fatalf("entries = %+v", by)
	}
	if c := f.call(t, "GET", "/api/fs/list?path="+q(f.home+"/../"), nil, nil); c != 403 {
		t.Fatalf("list above home: %d", c)
	}
	if c := f.call(t, "GET", "/api/fs/list?path="+q(f.home+"/repo/a.txt"), nil, nil); c != 400 {
		t.Fatalf("list of a file: %d", c)
	}
}

func TestFsRawServesRangesAndSandboxesHTML(t *testing.T) {
	f := newFsEnv(t)
	writeFile(t, filepath.Join(f.home, "page.html"), "<script>alert(1)</script>")
	writeFile(t, filepath.Join(f.home, "noext"), "<html><script>alert(1)</script>")
	writeFile(t, filepath.Join(f.home, "pic.png"), "\x89PNG\r\n\x1a\n0123456789")
	r := f.do(t, "GET", "/api/fs/raw?path="+q(f.home+"/pic.png"), "", map[string]string{"Cf-Access-Jwt-Assertion": f.jwt, "Cookie": "cp_session=" + f.tok, "Range": "bytes=8-11"})
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 206 || string(b) != "0123" || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("X-Mtime") == "" ||
		strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("range: %d %q %v", r.StatusCode, b, r.Header)
	}
	for _, p := range []string{"page.html", "noext"} {
		r = f.do(t, "GET", "/api/fs/raw?path="+q(f.home+"/"+p), "", f.authed(f.tok))
		if csp := r.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox") || !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Fatalf("%s served with CSP %q", p, csp)
		}
	}
	r = f.do(t, "GET", "/api/fs/raw?dl=1&path="+q(f.home+"/page.html"), "", f.authed(f.tok))
	if !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("dl=1: %v", r.Header)
	}
	if c := f.call(t, "GET", "/api/fs/raw?path=~", nil, nil); c != 400 {
		t.Fatalf("raw of a folder: %d", c)
	}
}

func TestFsWriteChecksTheVersionAndKeepsTheMode(t *testing.T) {
	f := newFsEnv(t)
	p := filepath.Join(f.home, "notes.txt")
	put := func(mtime, extra, body string) (int, map[string]any) {
		var v map[string]any
		return f.call(t, "PUT", "/api/fs/file?path="+q(p)+"&mtime="+mtime+extra, body, &v), v
	}
	c, v := put("0", "", "one")
	if c != 200 || v["size"].(float64) != 3 {
		t.Fatalf("create: %d %v", c, v)
	}
	mt := strconv.FormatInt(int64(v["mtime"].(float64)), 10)
	if c, v := put("0", "", "again"); c != 409 || v["error"] != "exists" {
		t.Fatalf("mtime=0 over an existing file: %d %v", c, v)
	}
	os.Chmod(p, 0o600)
	if c, v := put("12345", "", "stale"); c != 409 || v["error"] != "changed on disk" || strconv.FormatInt(int64(v["mtime"].(float64)), 10) != mt {
		t.Fatalf("stale version: %d %v", c, v)
	}
	if c, _ := put(mt, "", "two"); c != 200 {
		t.Fatalf("save: %d", c)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: a save keeps it", fi.Mode())
	}
	if c, _ := put("1", "&force=1", "three"); c != 200 {
		t.Fatalf("force: %d", c)
	}
	if ents, _ := os.ReadDir(f.home); slices.ContainsFunc(ents, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".tmp") }) {
		t.Fatal("a temp file was left behind")
	}
	if c := f.call(t, "PUT", "/api/fs/file?mtime=0&path="+q(f.home+"/../evil"), "x", nil); c != 403 {
		t.Fatalf("write outside: %d", c)
	}
}

// A page's comments are saved beside it on the page's own say-so (Files' preview bridge): a link a cloned repo
// planted at that name must be replaced, never written through (it could point at ~/.zshrc).
func TestFsWriteNolinkReplacesALinkInsteadOfWritingThroughIt(t *testing.T) {
	f := newFsEnv(t)
	victim := filepath.Join(f.home, "victim")
	os.WriteFile(victim, []byte("keep"), 0o644)
	store := filepath.Join(f.home, "page.html.comments.json")
	os.Symlink(victim, store)
	if c := f.call(t, "PUT", "/api/fs/file?force=1&nolink=1&path="+q(store), `{"comments":[]}`, nil); c != 200 {
		t.Fatalf("save: %d", c)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("written through the link: %q", b)
	}
	if fi, err := os.Lstat(store); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the link should now be a plain file: %v %v", fi, err)
	}
	// the editor still saves a linked file where the link points
	os.Remove(store)
	os.Symlink(victim, store)
	if c := f.call(t, "PUT", "/api/fs/file?force=1&path="+q(store), "edited", nil); c != 200 {
		t.Fatalf("editor save: %d", c)
	}
	if b, _ := os.ReadFile(victim); string(b) != "edited" {
		t.Fatalf("editor save through a link: %q", b)
	}
}

func (f *fsEnv) upload(t *testing.T, dir, extra string, files map[string]string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + name + `"`}
		pw, _ := mw.CreatePart(h)
		pw.Write([]byte(body))
	}
	mw.Close()
	hdr := f.authed(f.tok)
	hdr["Content-Type"] = mw.FormDataContentType()
	r := f.do(t, "POST", "/api/fs/upload?dir="+q(dir)+extra, buf.String(), hdr)
	var v map[string]any
	json.NewDecoder(r.Body).Decode(&v)
	return r.StatusCode, v
}

func TestFsUploadNeverOverwritesUnlessAsked(t *testing.T) {
	f := newFsEnv(t)
	files := map[string]string{"a.txt": "A", "folder/sub/b.txt": "B"}
	if c, v := f.upload(t, f.home, "", files); c != 200 || len(v["saved"].([]any)) != 2 {
		t.Fatalf("upload: %d %v", c, v)
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, "folder/sub/b.txt")); string(b) != "B" {
		t.Fatalf("nested: %q", b)
	}
	files["a.txt"], files["folder/sub/b.txt"] = "A2", "B2"
	if c, v := f.upload(t, f.home, "", files); c != 409 || len(v["exists"].([]any)) != 2 {
		t.Fatalf("again: %d %v", c, v)
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, "a.txt")); string(b) != "A" {
		t.Fatalf("overwritten without replace: %q", b)
	}
	if c, v := f.upload(t, f.home, "&replace=1", files); c != 200 || len(v["saved"].([]any)) != 2 {
		t.Fatalf("replace: %d %v", c, v)
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, "a.txt")); string(b) != "A2" {
		t.Fatalf("replace=1: %q", b)
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(f.home, "out"))
	for _, bad := range []string{"../escape.txt", "x/../../escape.txt", "/abs.txt", "out/sub/escape.txt"} {
		if c, _ := f.upload(t, f.home, "", map[string]string{bad: "x"}); c < 400 {
			t.Errorf("%s accepted: %d", bad, c)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 || fileExists(filepath.Join(filepath.Dir(f.home), "escape.txt")) {
		t.Fatal("an upload got out of the roots")
	}
}

func TestFsRenameMoveCopyDelete(t *testing.T) {
	f := newFsEnv(t)
	h := func(p string) string { return filepath.Join(f.home, p) }
	writeFile(t, h("a.txt"), "a")
	writeFile(t, h("b.txt"), "b")
	writeFile(t, h(".env"), "e")
	writeFile(t, h("dir/inner/c.txt"), "c")
	var v map[string]any

	if c := f.call(t, "POST", "/api/fs/rename", map[string]string{"path": h("a.txt"), "name": "b.txt"}, nil); c != 409 {
		t.Fatalf("rename onto an existing name: %d", c)
	}
	for _, n := range []string{"x/y", "..", "", "."} {
		if c := f.call(t, "POST", "/api/fs/rename", map[string]string{"path": h("a.txt"), "name": n}, nil); c != 400 {
			t.Fatalf("rename to %q: %d", n, c)
		}
	}
	if c := f.call(t, "POST", "/api/fs/rename", map[string]string{"path": h("a.txt"), "name": "z.txt"}, &v); c != 200 || v["path"] != h("z.txt") {
		t.Fatalf("rename: %d %v", c, v)
	}

	for _, want := range []string{"z copy.txt", "z copy 2.txt"} {
		if c := f.call(t, "POST", "/api/fs/copy", map[string]string{"path": h("z.txt")}, &v); c != 200 || v["path"] != h(want) {
			t.Fatalf("copy: %d %v, want %s", c, v, want)
		}
	}
	if f.call(t, "POST", "/api/fs/copy", map[string]string{"path": h(".env")}, &v); v["path"] != h(".env copy") {
		t.Fatalf("dotfile copy: %v", v)
	}
	if f.call(t, "POST", "/api/fs/copy", map[string]string{"path": h("dir")}, &v); v["path"] != h("dir copy") || !fileExists(h("dir copy/inner/c.txt")) {
		t.Fatalf("folder copy: %v", v)
	}

	writeFile(t, h("dir/b.txt"), "taken")
	var mv struct {
		Moved   []string
		Skipped []map[string]string
	}
	f.call(t, "POST", "/api/fs/move", map[string]any{"paths": []string{h("z.txt"), h("b.txt"), h("dir"), f.home}, "to": h("dir")}, &mv)
	if !slices.Equal(mv.Moved, []string{h("dir/z.txt")}) || len(mv.Skipped) != 3 || !fileExists(h("b.txt")) {
		t.Fatalf("move: %+v (a name taken, a folder into itself, a root: all skipped)", mv)
	}
	f.call(t, "POST", "/api/fs/move", map[string]any{"paths": []string{h("dir copy")}, "to": h("dir/inner")}, &mv)
	if !fileExists(h("dir/inner/dir copy/inner/c.txt")) {
		t.Fatalf("folder move: %+v", mv)
	}

	os.Symlink(h("dir"), h("link"))
	var del struct {
		Deleted []string
		Skipped []map[string]string
	}
	f.call(t, "POST", "/api/fs/delete", map[string]any{"paths": []string{f.home, filepath.Dir(f.home), "/", f.other, h("../x"), h("link"), h("missing")}}, &del)
	if !slices.Equal(del.Deleted, []string{h("link")}) || len(del.Skipped) != 6 || !fileExists(f.home) || !fileExists(h("dir/z.txt")) {
		t.Fatalf("delete: %+v (roots, their parents and outside refused; a link goes, not its target)", del)
	}
	f.call(t, "POST", "/api/fs/delete", map[string]any{"paths": []string{h("dir")}}, &del)
	if fileExists(h("dir")) {
		t.Fatalf("delete dir: %+v", del)
	}
	if b, _ := os.ReadFile(f.audit); !bytes.Contains(b, []byte("fs-delete")) || !bytes.Contains(b, []byte("fs-move")) || !bytes.Contains(b, []byte("fs-rename")) {
		t.Fatalf("audit log: %s", b)
	}
	if c := f.call(t, "POST", "/api/fs/mkdir", map[string]string{"path": h("new")}, nil); c != 200 || !fileExists(h("new")) {
		t.Fatalf("mkdir: %d", c)
	}
	if c := f.call(t, "POST", "/api/fs/mkdir", map[string]string{"path": h("new")}, nil); c != 409 {
		t.Fatalf("mkdir again: %d", c)
	}
}

func TestFsMoveAcrossFilesystems(t *testing.T) {
	shm, err := os.MkdirTemp("/dev/shm", "fs-test-")
	if err != nil {
		t.Skip("no /dev/shm")
	}
	defer os.RemoveAll(shm)
	src := filepath.Join(t.TempDir(), "d")
	writeFile(t, filepath.Join(src, "f.txt"), "x")
	if err := fsMove(src, filepath.Join(shm, "d")); err != nil || fileExists(src) || !fileExists(filepath.Join(shm, "d/f.txt")) {
		t.Fatalf("EXDEV move: %v", err)
	}
	writeFile(t, src+"/f.txt", "again")
	if err := fsMove(src, filepath.Join(shm, "d")); err == nil || !fileExists(src) {
		t.Fatalf("a move across filesystems must not replace: %v", err)
	}
}

func TestFsSearchAndGrep(t *testing.T) {
	f := newFsEnv(t)
	writeFile(t, filepath.Join(f.home, "docs/Rent Roll.xlsx"), "x")
	writeFile(t, filepath.Join(f.home, "odd:name.txt"), "a\nthe needle here\n")
	f.s.fsx.rebuild()
	var v struct {
		Items []struct {
			Path string
			Dir  bool
			Size int64
		}
		Total, Indexed int
	}
	if c := f.call(t, "GET", "/api/fs/search?q=roll+rent&in=~/docs", nil, &v); c != 200 || v.Total != 1 || v.Items[0].Path != filepath.Join(f.home, "docs/Rent Roll.xlsx") || v.Indexed == 0 {
		t.Fatalf("search: %d %+v", c, v)
	}
	if c := f.call(t, "GET", "/api/fs/search?q=x&kind=nope", nil, nil); c != 400 {
		t.Fatalf("bad kind: %d", c)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("no ripgrep")
	}
	var g struct {
		Items []struct {
			Path string
			Line int
			Text string
		}
	}
	if c := f.call(t, "GET", "/api/fs/grep?q=needle&in=~", nil, &g); c != 200 || len(g.Items) != 1 || g.Items[0].Line != 2 ||
		g.Items[0].Path != filepath.Join(f.home, "odd:name.txt") || g.Items[0].Text != "the needle here" {
		t.Fatalf("grep: %d %+v", c, g)
	}
}

func TestFsCloneStreamsProgressAndCleansUp(t *testing.T) {
	f := newFsEnv(t)
	src := filepath.Join(f.other, "src")
	for _, args := range [][]string{{"init", "-q", src}, {"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git: %v %s", err, out)
		}
	}
	clone := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		r := f.do(t, "POST", "/api/fs/clone", string(b), f.authed(f.tok))
		var last map[string]any
		for sc := bufio.NewScanner(r.Body); sc.Scan(); {
			json.Unmarshal(sc.Bytes(), &last)
		}
		return last
	}
	if v := clone(map[string]any{"url": src, "dir": f.home}); v["ok"] != true || !fileExists(filepath.Join(f.home, "src/.git")) {
		t.Fatalf("clone: %v", v)
	}
	if v := clone(map[string]any{"url": filepath.Join(f.other, "nope"), "dir": f.home, "name": "broken"}); v["error"] == nil || fileExists(filepath.Join(f.home, "broken")) {
		t.Fatalf("a failed clone reports and leaves nothing: %v", v)
	}
	for _, body := range []map[string]any{{"url": src, "dir": f.home}, {"url": "--upload-pack=x", "dir": f.home}, {"url": src, "dir": f.home, "name": "../x"}} {
		if c := f.call(t, "POST", "/api/fs/clone", body, nil); c != 409 && c != 400 {
			t.Fatalf("clone %v: %d", body, c)
		}
	}
}

func TestLocalClonesReadsOriginRemotes(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "repos/a/.git/config"), "[remote \"upstream\"]\n\turl = https://github.com/x/y\n[remote \"origin\"]\n\turl = git@github.com:Owner/Repo-A.git\n")
	writeFile(t, filepath.Join(home, "projects/b/.git/config"), "[remote \"origin\"]\n\turl = https://github.com/owner/b\n")
	m := localClones(home)
	if m["owner/repo-a"] != filepath.Join(home, "repos/a") || m["owner/b"] != filepath.Join(home, "projects/b") || len(m) != 2 {
		t.Fatalf("clones = %v", m)
	}
}

func TestFsDeleteRefusesWhatThePanelNeeds(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id"), []byte("k"), 0o600)
	g := deleteGuard{protect: []string{filepath.Join(home, ".ssh")}}
	for _, p := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, ".ssh", "id"), home} {
		if g.protected(p) == nil {
			t.Errorf("%s: want refused", p)
		}
	}
	if err := g.protected(filepath.Join(home, "notes.txt")); err != nil {
		t.Errorf("an ordinary file is refused: %v", err)
	}
}

func TestFsRawIsNeverThePanelsScript(t *testing.T) {
	f := newFsEnv(t)
	p := filepath.Join(f.home, "evil.js")
	writeFile(t, p, "alert(1)")
	hdr := f.authed(f.tok)
	hdr["Sec-Fetch-Dest"] = "script"
	if r := f.do(t, "GET", "/api/fs/raw?path="+q(p), "", hdr); r.StatusCode != 403 {
		t.Errorf("loaded as a script: %d", r.StatusCode)
	}
	delete(hdr, "Sec-Fetch-Dest")
	if r := f.do(t, "GET", "/api/fs/raw?path="+q(p), "", hdr); r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("a .js file is shown as text: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if mime.TypeByExtension(".es") == "application/ecmascript" { // /usr/share/mime says so: browsers run it as JavaScript
		writeFile(t, p+".es", "alert(1)")
		if r := f.do(t, "GET", "/api/fs/raw?path="+q(p+".es"), "", hdr); !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("an .es file is served as %s", r.Header.Get("Content-Type"))
		}
	}
}

func TestFsCloneOnlyFromRemotesOrInsideTheRoots(t *testing.T) {
	f := newFsEnv(t)
	outside := t.TempDir()
	for _, u := range []string{outside, "file://" + outside, "../" + filepath.Base(outside)} {
		if c := f.call(t, "POST", "/api/fs/clone", map[string]any{"url": u, "dir": f.home, "name": "x"}, nil); c == 200 {
			t.Errorf("clone from %s: %d", u, c)
		}
	}
}

func TestFsNaturalOrder(t *testing.T) {
	var es []fsEntry
	for _, n := range []string{"file-10.txt", "File-2.txt", "b", "file-1.txt", "a10", "a9", "zdir"} {
		es = append(es, fsEntry{Name: n, Dir: n == "zdir" || n == "b", key: strings.ToLower(n)})
	}
	slices.SortFunc(es, fsNatural)
	var got []string
	for _, e := range es {
		got = append(got, e.Name)
	}
	if want := "b zdir a9 a10 file-1.txt File-2.txt file-10.txt"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

// Text goes gzipped (by type, or by its bytes when its name says nothing or something else), whole or as its first or
// last n bytes; ranges and binary files go as they are; and an unchanged file is a 304.
func TestFsRawGzipsTextAndSlicesIt(t *testing.T) {
	f := newFsEnv(t)
	log := strings.Repeat("2026-10-12 21:40:01 INFO request served in 3 ms\n", 400)
	for name, body := range map[string]string{"app.log": log, "Makefile": log, "types.ts": log, "pic.png": "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01", 2000)} {
		os.WriteFile(filepath.Join(f.home, name), []byte(body), 0o644)
	}
	get := func(name, extra string, hdr map[string]string) (*http.Response, string) {
		h := f.authed(f.tok)
		for k, v := range hdr {
			h[k] = v
		}
		r := f.do(t, "GET", "/api/fs/raw?path="+q(filepath.Join(f.home, name))+extra, "", h)
		var rd io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			rd, _ = gzip.NewReader(r.Body)
		}
		b, _ := io.ReadAll(rd)
		return r, string(b)
	}
	gz := map[string]string{"Accept-Encoding": "gzip"}
	for _, name := range []string{"app.log", "Makefile", "types.ts"} {
		if r, b := get(name, "", gz); r.Header.Get("Content-Encoding") != "gzip" || b != log {
			t.Fatalf("%s: encoding %q, %d bytes", name, r.Header.Get("Content-Encoding"), len(b))
		}
	}
	if r, b := get("app.log", "&tail=49", gz); b != log[len(log)-49:] || r.Header.Get("X-Total") != strconv.Itoa(len(log)) {
		t.Fatalf("tail: %q total %s", b, r.Header.Get("X-Total"))
	}
	if _, b := get("app.log", "&head=2000", gz); b != log[:2000] {
		t.Fatal("head")
	}
	if r, _ := get("pic.png", "", gz); r.Header.Get("Content-Encoding") != "" {
		t.Fatal("a picture is already compressed")
	}
	if r, b := get("app.log", "", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"}); r.StatusCode != 206 || b != log[:10] {
		t.Fatalf("range: %d %q", r.StatusCode, b)
	}
	r, _ := get("app.log", "", gz)
	if r2, _ := get("app.log", "", map[string]string{"Accept-Encoding": "gzip", "If-Modified-Since": r.Header.Get("Last-Modified")}); r2.StatusCode != 304 {
		t.Fatalf("unchanged: %d", r2.StatusCode)
	}
}

// How long an open took, from the device that did it: a line each, kinds and times only.
func TestFsTimingKeepsALineAnOpen(t *testing.T) {
	f := newFsEnv(t)
	if code := f.call(t, "POST", "/api/fs/timing", map[string]any{"what": "rows", "ext": "xlsx", "ms": 812, "size": 11777665, "narrow": true, "net": "4g", "rtt": 100}, nil); code != 204 {
		t.Fatalf("%d", code)
	}
	if code := f.call(t, "POST", "/api/fs/timing", map[string]any{"what": strings.Repeat("x", 40), "ms": 1}, nil); code != 400 {
		t.Fatalf("a long kind: %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(f.s.cfg.auditFile), "files-timings.jsonl"))
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &got); err != nil || got["ms"] != 812.0 || got["ext"] != "xlsx" || got["narrow"] != true {
		t.Fatalf("%v %s", err, b)
	}
}
