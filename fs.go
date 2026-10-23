package main

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// ---- Files: browse, edit, upload, move and search everything under home and /tmp ----

var (
	errFsOutside = errors.New("only paths inside your home folder or /tmp")
	errFsRoot    = errors.New("that is a root folder (home or /tmp), or holds one")
	errFsName    = errors.New("not a valid name")
)

// fsResolve maps a client path ("~/" is roots[0], home) to a real path inside one of roots. follow resolves the
// path's own symlink too (reading, writing through a link); without it only the folder is resolved, so delete,
// rename and move act on a link itself, never on what it points to. A path that doesn't exist yet resolves by its folder.
func fsResolve(roots []string, p string, follow bool) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = roots[0] + p[1:]
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("not an absolute path")
	}
	p = filepath.Clean(p)
	real, err := "", error(nil)
	if follow {
		real, err = filepath.EvalSymlinks(p)
	}
	if !follow || errors.Is(err, fs.ErrNotExist) {
		var dir string
		if dir, err = filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			real = filepath.Join(dir, filepath.Base(p))
		}
	}
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(roots, func(r string) bool { return real == r || strings.HasPrefix(real, r+"/") }) {
		return "", errFsOutside
	}
	return real, nil
}

func (s *server) fsPath(p string, follow bool) (string, error) {
	return fsResolve(s.fsx.roots, p, follow)
}

// fsTarget resolves an entry to delete, move, rename or copy: it must exist, and be neither a root nor above one.
func (s *server) fsTarget(p string) (string, error) {
	real, err := s.fsPath(p, false)
	if err == nil && slices.ContainsFunc(s.fsx.roots, func(r string) bool { return real == r || strings.HasPrefix(r, real+"/") }) {
		err = errFsRoot
	}
	if err == nil {
		_, err = os.Lstat(real)
	}
	return real, err
}

// fsMovable is fsTarget for what takes a path away (delete, move, rename): never what the panel, its tunnel, SSH,
// Claude or a CI runner needs (the disk explorer's guard). Reading and editing those stays open.
func (s *server) fsMovable(p string) (string, error) {
	real, err := s.fsTarget(p)
	if err == nil {
		err = s.deleteGuard().protected(real)
	}
	return real, err
}

func fsStatus(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 404
	case errors.Is(err, fs.ErrExist):
		return 409
	case errors.Is(err, errFsOutside), errors.Is(err, errFsRoot), errors.Is(err, fs.ErrPermission):
		return 403
	}
	return 400
}

func fsFail(w http.ResponseWriter, err error) {
	writeJSON(w, fsStatus(err), map[string]any{"error": err.Error()})
}

func fsName(n string) bool {
	return n != "" && n != "." && n != ".." && len(n) <= 255 && !strings.ContainsAny(n, "/\x00")
}

// findBin is name on PATH, else fallback if it's there, else "".
func findBin(name, fallback string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return ""
}

// ---- listing and reading ----

const fsListMax = 20000

type fsEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Link  bool   `json:"link,omitempty"`
	Br    string `json:"br,omitempty"` // a git checkout's branch (or short sha)
	key   string // the name lowercased, for fsNatural
}

// fsNatural orders folders first, then names the way people sort them: case aside, digit runs as numbers (file-2
// before file-10). The server sorts once so a 20k folder isn't sorted again on every phone that opens it.
func fsNatural(a, b fsEntry) int {
	if a.Dir != b.Dir {
		if a.Dir {
			return -1
		}
		return 1
	}
	x, y := a.key, b.key
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	for x != "" && y != "" {
		if digit(x[0]) && digit(y[0]) {
			i, j := 1, 1
			for i < len(x) && digit(x[i]) {
				i++
			}
			for j < len(y) && digit(y[j]) {
				j++
			}
			nx, ny := strings.TrimLeft(x[:i], "0"), strings.TrimLeft(y[:j], "0")
			if c := cmp.Or(cmp.Compare(len(nx), len(ny)), strings.Compare(nx, ny)); c != 0 {
				return c
			}
			x, y = x[i:], y[j:]
			continue
		}
		if x[0] != y[0] {
			return cmp.Compare(x[0], y[0])
		}
		x, y = x[1:], y[1:]
	}
	return cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(a.Name, b.Name))
}

func (s *server) fsList(w http.ResponseWriter, r *http.Request) {
	dir, err := s.fsPath(r.URL.Query().Get("path"), true)
	if err != nil {
		fsFail(w, err)
		return
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0) // a folder or nothing: a fifo can't block it
	if err != nil {
		fsFail(w, err)
		return
	}
	defer f.Close()
	ents, err := f.ReadDir(fsListMax + 1)
	if err != nil && err != io.EOF {
		fsFail(w, err)
		return
	}
	more := len(ents) > fsListMax
	ents = ents[:min(len(ents), fsListMax)]
	out := make([]fsEntry, len(ents))
	stat := func(i int) {
		e := ents[i]
		fi, err := e.Info()
		if err != nil {
			return // gone since the listing: dropped below
		}
		full := filepath.Join(dir, e.Name())
		it := fsEntry{Name: e.Name(), Link: fi.Mode()&fs.ModeSymlink != 0, key: strings.ToLower(e.Name())}
		if it.Link { // a link to a folder lists as a folder
			if t, err := os.Stat(full); err == nil {
				fi = t
			}
		}
		it.Dir, it.Size, it.Mtime = fi.IsDir(), fi.Size(), fi.ModTime().UnixMilli()
		if it.Dir && fileExists(full+"/.git") {
			it.Br = gitInfo(full).Branch
		}
		out[i] = it
	}
	if n, k := len(ents), runtime.NumCPU(); n < 512 {
		for i := range ents {
			stat(i)
		}
	} else { // a big folder: its stats spread over the cores
		var wg sync.WaitGroup
		for c := range k {
			wg.Go(func() {
				for i := c; i < n; i += k {
					stat(i)
				}
			})
		}
		wg.Wait()
	}
	out = slices.DeleteFunc(out, func(e fsEntry) bool { return e.Name == "" })
	slices.SortFunc(out, fsNatural)
	var st syscall.Statfs_t // the free space of the disk the folder is on, for the tree's footer
	free := int64(0)
	if syscall.Statfs(dir, &st) == nil {
		free = int64(st.Bavail) * int64(st.Bsize)
	}
	body := map[string]any{"path": dir, "entries": out, "more": more, "free": free}
	if len(out) > 300 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") { // ~10× smaller through the tunnel
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		json.NewEncoder(gz).Encode(body)
		gz.Close()
		return
	}
	writeJSON(w, 200, body)
}

// the sandbox paneFile's raw mode serves HTML and SVG in
const fsSandboxCSP = "sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox; default-src 'none'; " +
	"script-src 'unsafe-inline' https:; style-src 'unsafe-inline' https: data:; img-src https: data: blob:; font-src https: data:; " +
	"media-src https: data:; connect-src 'none'; frame-ancestors 'self'"

// fsRaw serves a file's bytes (ranges too: video seeking, pdf.js). Images, media and PDFs can't script; anything
// else (HTML, SVG, whatever a sniff calls HTML) renders, if at all, in an opaque origin the panel may frame.
func (s *server) fsRaw(w http.ResponseWriter, r *http.Request) {
	switch r.Header.Get("Sec-Fetch-Dest") { // a file here is never a script, style or worker of the panel's own pages
	case "script", "style", "worker", "sharedworker", "serviceworker", "audioworklet", "paintworklet":
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, err := s.fsPath(r.URL.Query().Get("path"), true)
	if err != nil {
		fsFail(w, err)
		return
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) // a fifo can't block the open
	if err != nil {
		fsFail(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeJSON(w, 400, map[string]any{"error": "not a file"})
		return
	}
	h := w.Header()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
	if strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript") || strings.HasPrefix(ct, "text/css") { // shown as text, never run as one of the panel's
		ct = "text/plain; charset=utf-8"
	}
	if ct != "" {
		h.Set("Content-Type", ct) // else ServeContent sniffs
	}
	csp := fsSandboxCSP
	if ct == "application/pdf" || strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "image/") && !strings.Contains(ct, "svg") {
		csp = "frame-ancestors 'self'"
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	disp := "inline"
	if r.URL.Query().Get("dl") == "1" {
		disp = "attachment"
	}
	h.Set("Content-Disposition", cmp.Or(mime.FormatMediaType(disp, map[string]string{"filename": filepath.Base(p)}), disp))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Mtime", strconv.FormatInt(fi.ModTime().UnixMilli(), 10))
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" { // ranges are of the bytes as they are
		http.ServeContent(w, r, p, fi.ModTime(), f)
		return
	}
	// ?head=n / ?tail=n: only the first or last n bytes (a big text file opens on its first or last 5 MB)
	off, n, q := int64(0), fi.Size(), r.URL.Query()
	if v, err := strconv.ParseInt(q.Get("head"), 10, 64); err == nil && v >= 0 && v < n {
		n = v
	} else if v, err := strconv.ParseInt(q.Get("tail"), 10, 64); err == nil && v >= 0 && v < n {
		off, n = n-v, v
	}
	h.Set("X-Total", strconv.FormatInt(fi.Size(), 10))
	sniff := make([]byte, min(n, 512))
	f.ReadAt(sniff, off)
	if ct == "" {
		ct = http.DetectContentType(sniff)
		h.Set("Content-Type", ct)
	}
	// text, code, CSV, JSON, SVG: gzipped on the way (2–10× smaller through the tunnel; at BestSpeed ~0.1 s for 5 MB);
	// anything else only when asked (&gz=1: a PDF read whole, a third smaller; pdf.js reading by ranges gives up on
	// ranges if its first request comes back encoded)
	if n > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && (q.Get("gz") == "1" || fsSqueezes(ct, http.DetectContentType(sniff))) {
		h.Set("Vary", "Accept-Encoding")
		h.Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
		if t, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !fi.ModTime().Truncate(time.Second).After(t) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Encoding", "gzip")
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		io.Copy(gz, io.NewSectionReader(f, off, n))
		gz.Close()
		return
	}
	http.ServeContent(w, r, p, fi.ModTime(), io.NewSectionReader(f, off, n))
}

// fsSqueezes: worth gzipping, by its type or, whatever its name says (.ts is "video/mp2t"), by what its bytes look like.
func fsSqueezes(ct, sniffed string) bool {
	for _, t := range []string{"text/", "json", "xml", "javascript", "csv", "yaml", "toml", "x-sh", "svg"} {
		if strings.Contains(ct, t) {
			return true
		}
	}
	return strings.HasPrefix(sniffed, "text/")
}

// ---- writing ----

var fsWriteMu sync.Mutex // a save's version check and its rename are one step

// fsWrite saves an edited file atomically (temp file, fsync, rename), refusing when it changed on disk since
// the editor loaded it (?mtime=, its version) unless &force=1; mtime=0 creates a new file and never replaces one.
// &nolink=1 replaces a symlink at path instead of writing where it points (a file a page asked for, by its name).
func (s *server) fsWrite(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.fsPath(q.Get("path"), q.Get("nolink") != "1")
	if err != nil {
		fsFail(w, err)
		return
	}
	want, err := strconv.ParseInt(q.Get("mtime"), 10, 64)
	force := q.Get("force") == "1"
	if err != nil && !force {
		writeJSON(w, 400, map[string]any{"error": "mtime: the version you edited, or 0 for a new file"})
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		fsFail(w, err)
		return
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = io.Copy(tmp, http.MaxBytesReader(w, r.Body, 64<<20))
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if _, big := err.(*http.MaxBytesError); big {
		writeJSON(w, 413, map[string]any{"error": "files up to 64 MB"})
		return
	} else if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.fsReplace(w, r, p, tmp.Name(), want, force)
}

// fsReplace puts tmp (written, synced, closed) in p's place, keeping p's mode, if p is still the version the client
// edited (want: its mtime; 0 for a new file); it answers the request.
func (s *server) fsReplace(w http.ResponseWriter, r *http.Request, p, tmp string, want int64, force bool) {
	fsWriteMu.Lock()
	defer fsWriteMu.Unlock()
	mode, cur := fs.FileMode(0o644), int64(0)
	fi, err := os.Stat(p)
	exists := err == nil
	if exists {
		if fi.IsDir() {
			writeJSON(w, 400, map[string]any{"error": "that is a folder"})
			return
		}
		mode, cur = fi.Mode().Perm(), fi.ModTime().UnixMilli()
	}
	if !force && want == 0 && exists {
		writeJSON(w, 409, map[string]any{"error": "exists", "mtime": cur})
		return
	}
	if !force && want != 0 && (!exists || cur != want) {
		writeJSON(w, 409, map[string]any{"error": "changed on disk", "mtime": cur})
		return
	}
	err = os.Chmod(tmp, mode)
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err == nil {
		fi, err = os.Stat(p)
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.audit.add("fs-write", fmt.Sprintf("%s (%s)", trunc(tilde(p), 300), fmtBytes(fi.Size())), clientIP(r))
	if !exists {
		s.fsx.note(p)
	}
	writeJSON(w, 200, map[string]any{"mtime": fi.ModTime().UnixMilli(), "size": fi.Size()})
}

const fsUploadMax = 8 << 30 // ponytail: a per-file cap, not a quota; a full disk fails the write and its temp goes

// fsUpload streams a multipart upload into ?dir=. A part's filename may be a relative path (a dropped folder):
// its folders are made. An existing file is left alone and reported, unless &replace=1.
func (s *server) fsUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, err := s.fsPath(q.Get("dir"), true)
	if err != nil {
		fsFail(w, err)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	saved, exists := []map[string]any{}, []string{}
	var total int64
	defer func() {
		if len(saved) > 0 {
			s.audit.add("fs-upload", fmt.Sprintf("%d file(s), %s, into %s", len(saved), fmtBytes(total), trunc(tilde(dir), 300)), clientIP(r))
			var made []string
			for _, f := range saved {
				rel := f["name"].(string)
				made = append(made, filepath.Join(dir, rel), filepath.Join(dir, strings.SplitN(rel, "/", 2)[0])) // the file, and a dropped folder's top
			}
			s.fsx.note(made...)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error(), "saved": saved, "exists": exists})
			return
		}
		_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition")) // not part.FileName(): it drops the folders
		rel := params["filename"]
		if rel == "" {
			continue // a plain form field
		}
		n, err := s.fsSave(dir, rel, part, q.Get("replace") == "1")
		switch {
		case errors.Is(err, fs.ErrExist):
			exists = append(exists, rel)
		case err != nil:
			writeJSON(w, fsStatus(err), map[string]any{"error": rel + ": " + err.Error(), "saved": saved, "exists": exists})
			return
		default:
			saved, total = append(saved, map[string]any{"name": rel, "size": n}), total+n
		}
	}
	code := 200
	if len(saved) == 0 {
		code = 409
		if len(exists) == 0 {
			code = 400
		}
	}
	writeJSON(w, code, map[string]any{"saved": saved, "exists": exists})
}

// fsSave writes one upload to dir/rel through a temp file, making rel's folders one at a time so a link among
// them can't lead out of the roots. Without replace a hard link puts it in place: it can't clobber what's there.
func (s *server) fsSave(dir, rel string, body io.Reader, replace bool) (int64, error) {
	if path.IsAbs(rel) || slices.Contains(strings.Split(rel, "/"), "..") || path.Clean(rel) == "." {
		return 0, errFsName
	}
	parts := strings.Split(path.Clean(rel), "/")
	d := dir
	for _, c := range parts[:len(parts)-1] {
		if err := os.Mkdir(d+"/"+c, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return 0, err
		}
		var err error
		if d, err = s.fsPath(d+"/"+c, true); err != nil {
			return 0, err
		}
	}
	f, err := os.CreateTemp(d, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(body, fsUploadMax+1))
	if err == nil && n > fsUploadMax {
		err = errors.New("files up to 8 GB")
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if dst := d + "/" + parts[len(parts)-1]; err == nil && replace {
		err = os.Rename(f.Name(), dst)
	} else if err == nil {
		err = os.Link(f.Name(), dst)
	}
	return n, err
}

// ---- making, renaming, moving, copying, deleting ----

// fsTiming keeps how long Files took to open something, from the device that opened it (a phone over its real link,
// not a simulation): one JSON line each in files-timings.jsonl beside the audit log, ~1 MB then the older half goes.
// Kinds, sizes and times only, never a path.
var fsTimingMu sync.Mutex

func (s *server) fsTiming(w http.ResponseWriter, r *http.Request) {
	var t struct {
		What   string `json:"what"` // open, rows (a sheet's first), image, page (a PDF's first)
		Ext    string `json:"ext"`
		Ms     int    `json:"ms"`
		Size   int64  `json:"size"`
		Narrow bool   `json:"narrow"` // a phone's layout
		Net    string `json:"net"`    // the browser's guess: 4g, 3g…
		Rtt    int    `json:"rtt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&t); err != nil || t.Ms < 0 || len(t.What) > 16 || len(t.Ext) > 16 || len(t.Net) > 16 {
		writeJSON(w, 400, map[string]any{"error": "a timing: {what, ext, ms, size, narrow, net, rtt}"})
		return
	}
	line, _ := json.Marshal(map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "what": t.What, "ext": t.Ext, "ms": t.Ms, "size": t.Size,
		"narrow": t.Narrow, "net": t.Net, "rtt": t.Rtt, "mobile": strings.Contains(r.UserAgent(), "Mobile")})
	p := filepath.Join(filepath.Dir(s.cfg.auditFile), "files-timings.jsonl")
	fsTimingMu.Lock()
	defer fsTimingMu.Unlock()
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 { // the newer half stays
		if b, err := os.ReadFile(p); err == nil {
			b = b[len(b)/2:]
			os.WriteFile(p, b[bytes.IndexByte(b, '\n')+1:], 0o600)
		}
	}
	if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	w.WriteHeader(204)
}

func (s *server) fsMkdir(w http.ResponseWriter, r *http.Request) {
	var b struct{ Path string }
	if readJSONN(r, &b, 8<<10) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	p, err := s.fsPath(b.Path, false)
	if err == nil {
		err = os.Mkdir(p, 0o755)
	}
	if err != nil {
		fsFail(w, err)
		return
	}
	s.fsx.note(p)
	writeJSON(w, 200, map[string]any{"path": p})
}

// fsMove renames src to dst and never replaces anything; across filesystems (home and /tmp) mv copies it over.
func fsMove(src, dst string) error {
	err := renameNoReplace(src, dst)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) { // a filesystem without RENAME_NOREPLACE
		if _, e := os.Lstat(dst); e == nil {
			return fmt.Errorf("%s: %w", filepath.Base(dst), fs.ErrExist)
		}
		err = os.Rename(src, dst)
	}
	if errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("%s: %w", filepath.Base(dst), fs.ErrExist)
	}
	if errors.Is(err, syscall.EXDEV) { // another filesystem: mv copies; -n -T never over, never into, what's at dst
		if _, e := os.Lstat(dst); e == nil {
			return fmt.Errorf("%s: %w", filepath.Base(dst), fs.ErrExist)
		}
		out, e := exec.Command("mv", "-n", "-T", "--", src, dst).CombinedOutput()
		if _, err = os.Lstat(src); err == nil { // still there: mv failed, or -n saw dst appear
			return fmt.Errorf("mv: %v %s", e, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return err
}

// the syscall package names no renameat2; 0 (an unlisted arch) falls back to a check, then rename
var sysRenameat2 = map[string]uintptr{"amd64": 316, "arm64": 276, "riscv64": 276, "386": 353}[runtime.GOARCH]

// renameNoReplace is rename(2) that fails with EEXIST rather than replace what appeared at dst (renameat2 RENAME_NOREPLACE).
func renameNoReplace(src, dst string) error {
	s, err := syscall.BytePtrFromString(src)
	if err != nil {
		return err
	}
	d, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	if sysRenameat2 == 0 {
		return syscall.ENOSYS
	}
	at := -100 // AT_FDCWD
	if _, _, e := syscall.Syscall6(sysRenameat2, uintptr(at), uintptr(unsafe.Pointer(s)), uintptr(at), uintptr(unsafe.Pointer(d)), 1, 0); e != 0 {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: e}
	}
	return nil
}

func (s *server) fsRename(w http.ResponseWriter, r *http.Request) {
	var b struct{ Path, Name string }
	if readJSONN(r, &b, 8<<10) != nil || !fsName(b.Name) {
		writeJSON(w, 400, map[string]any{"error": "want {path, name}, name a single file or folder name"})
		return
	}
	src, err := s.fsMovable(b.Path)
	dst := filepath.Join(filepath.Dir(src), b.Name)
	if err == nil {
		err = fsMove(src, dst)
	}
	if err != nil {
		fsFail(w, err)
		return
	}
	s.audit.add("fs-rename", trunc(tilde(src), 300)+" → "+trunc(b.Name, 100), clientIP(r))
	s.fsx.note(dst)
	writeJSON(w, 200, map[string]any{"path": dst})
}

func (s *server) fsMoveTo(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Paths []string
		To    string
	}
	if readJSONN(r, &b, 1<<20) != nil || len(b.Paths) == 0 {
		writeJSON(w, 400, map[string]any{"error": "want {paths: [...], to}"})
		return
	}
	to, err := s.fsPath(b.To, true)
	if fi, e := os.Stat(to); err == nil && (e != nil || !fi.IsDir()) {
		err = errors.New("not a folder")
	}
	if err != nil {
		fsFail(w, err)
		return
	}
	moved, skipped := []string{}, []map[string]string{}
	for _, p := range b.Paths {
		src, err := s.fsMovable(p)
		if err == nil && (to == src || strings.HasPrefix(to, src+"/")) {
			err = errors.New("can't move a folder into itself")
		}
		dst := filepath.Join(to, filepath.Base(src))
		if err == nil {
			err = fsMove(src, dst)
		}
		if err != nil {
			skipped = append(skipped, map[string]string{"path": p, "error": err.Error()})
			continue
		}
		s.audit.add("fs-move", trunc(tilde(src)+" → "+tilde(to), 300), clientIP(r))
		moved = append(moved, dst)
	}
	if len(moved) > 0 {
		s.fsx.note(moved...)
	}
	writeJSON(w, 200, map[string]any{"moved": moved, "skipped": skipped})
}

// fsCopy duplicates a file or folder beside itself: "name copy.ext", then "name copy 2.ext"...
func (s *server) fsCopy(w http.ResponseWriter, r *http.Request) {
	var b struct{ Path string }
	if readJSONN(r, &b, 8<<10) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	src, err := s.fsTarget(b.Path)
	if err != nil {
		fsFail(w, err)
		return
	}
	stem, ext := filepath.Base(src), ""
	if fi, _ := os.Lstat(src); !fi.IsDir() && strings.TrimSuffix(stem, filepath.Ext(stem)) != "" { // ".env" has no extension
		ext = filepath.Ext(stem)
		stem = strings.TrimSuffix(stem, ext)
	}
	dst := ""
	for i := 1; ; i++ {
		n := stem + " copy" + ext
		if i > 1 {
			n = fmt.Sprintf("%s copy %d%s", stem, i, ext)
		}
		if dst = filepath.Join(filepath.Dir(src), n); !fileExists(dst) {
			break
		}
	}
	if out, err := exec.Command("cp", "-a", "-n", "-T", "--", src, dst).CombinedOutput(); err != nil { // -n -T: never over, never into, what appeared
		writeJSON(w, 500, map[string]any{"error": "cp: " + trunc(strings.TrimSpace(string(out)), 300)})
		return
	}
	s.audit.add("fs-copy", trunc(tilde(src)+" → "+filepath.Base(dst), 300), clientIP(r))
	s.fsx.note(dst)
	writeJSON(w, 200, map[string]any{"path": dst})
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (s *server) fsDelete(w http.ResponseWriter, r *http.Request) {
	var b struct{ Paths []string }
	if readJSONN(r, &b, 1<<20) != nil || len(b.Paths) == 0 {
		writeJSON(w, 400, map[string]any{"error": "want {paths: [...]}"})
		return
	}
	deleted, skipped := []string{}, []map[string]string{}
	for _, p := range b.Paths {
		real, err := s.fsMovable(p)
		var freed int64
		if err == nil {
			freed = s.du.sizeOf(real)
			err = os.RemoveAll(real)
		}
		if err != nil {
			skipped = append(skipped, map[string]string{"path": p, "error": err.Error()})
			continue
		}
		s.du.forget(real, freed) // the disk chart drops it now, not at the next scan
		s.audit.add("fs-delete", fmt.Sprintf("%s (%s)", trunc(tilde(real), 300), fmtBytes(freed)), clientIP(r))
		deleted = append(deleted, real)
	}
	if len(deleted) > 0 {
		s.fsx.note() // gone ones drop out of results at once (each hit is Lstat'ed); the rebuild forgets them
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted, "skipped": skipped})
}

// ---- search: names from the index, contents with ripgrep ----

func (s *server) fsSearch(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	q := r.URL.Query()
	sn := s.fsx.cur.Load()
	out := map[string]any{"items": []any{}, "total": 0, "building": s.fsx.building()}
	if sn == nil {
		go s.fsx.rebuild()
		out["building"] = true
		writeJSON(w, 200, out)
		return
	}
	out["indexed"], out["at"] = len(sn.parent), sn.at
	limit, _ := strconv.Atoi(q.Get("limit"))
	limit = min(cmp.Or(max(limit, 0), 50), 200)
	fq := fsQuery{terms: fsTerms(q.Get("q")), kind: q.Get("kind"), hidden: q.Get("hidden") == "1", in: -1, limit: 2 * limit} // the spare half: gone files, recency
	if _, ok := fsKinds[fq.kind]; !ok && fq.kind != "" {
		writeJSON(w, 400, map[string]any{"error": "kind is one of docs, sheets, code, images, media, folders"})
		return
	}
	if in := q.Get("in"); in != "" {
		p, err := s.fsPath(in, true)
		if err != nil {
			fsFail(w, err)
			return
		}
		if fq.in = sn.find(p); fq.in < 0 {
			writeJSON(w, 200, out) // not indexed yet
			return
		}
		fq.hidden = fq.hidden || sn.flags[fq.in]&fsHidden != 0 // searching inside ~/.config means its files
	}
	hits, total := sn.search(fq)
	inPath := ""
	if fq.in >= 0 {
		inPath = sn.path(fq.in)
	}
	fresh := s.fsx.freshHits(fq, inPath)
	type item struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Dir   bool   `json:"dir"`
		Size  int64  `json:"size"`
		Mtime int64  `json:"mtime"`
		score int
	}
	items := []item{}
	for _, h := range hits {
		p := sn.path(h.id)
		fi, err := os.Lstat(p)
		if err != nil {
			total--
			continue
		}
		if age := time.Since(fi.ModTime()); !fi.IsDir() && age < 7*24*time.Hour && age >= 0 {
			h.score += int(60 * (1 - age.Hours()/(7*24)))
		}
		items = append(items, item{p, filepath.Base(p), fi.IsDir(), fi.Size(), fi.ModTime().UnixMilli(), h.score})
	}
	for _, h := range fresh { // made through Files since the last walk
		if slices.ContainsFunc(items, func(it item) bool { return it.Path == h.path }) {
			continue
		}
		if fi, err := os.Lstat(h.path); err == nil {
			items = append(items, item{h.path, filepath.Base(h.path), fi.IsDir(), fi.Size(), fi.ModTime().UnixMilli(), h.score + 60})
			total++
		}
	}
	slices.SortStableFunc(items, func(a, b item) int { return b.score - a.score })
	out["items"], out["total"] = items[:min(len(items), limit)], total
	out["ms"] = math.Round(float64(time.Since(t0).Microseconds())/10) / 100
	writeJSON(w, 200, out)
}

// fsGrep searches file contents with ripgrep: literal, smart case, up to 200 matches in 8 s.
func (s *server) fsGrep(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	q := r.URL.Query()
	pat := q.Get("q")
	if pat == "" {
		writeJSON(w, 400, map[string]any{"error": "q is empty"})
		return
	}
	rg := findBin("rg", filepath.Join(s.cfg.home, ".pixi/bin/rg"))
	if rg == "" {
		writeJSON(w, 501, map[string]any{"error": "ripgrep isn't installed"})
		return
	}
	dirs := s.fsx.roots
	if in := q.Get("in"); in != "" {
		p, err := s.fsPath(in, true)
		if err != nil {
			fsFail(w, err)
			return
		}
		dirs = []string{p}
	}
	args := []string{"-n", "--no-heading", "--null", "--color", "never", "-S", "-F", "--max-count", "5", "--max-filesize", "4M",
		"--max-columns", "300", "--max-columns-preview", "-g", "!.git"}
	if q.Get("hidden") == "1" {
		args = append(args, "--hidden")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, rg, append(append(args, "--", pat), dirs...)...)
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	type item struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	items, truncated := []item{}, false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() { // path NUL line:text (--null: a path may hold ':')
		p, rest, _ := strings.Cut(sc.Text(), "\x00")
		ln, text, _ := strings.Cut(rest, ":")
		n, err := strconv.Atoi(ln)
		if err != nil {
			continue
		}
		if len(items) == 200 {
			truncated = true
			break
		}
		items = append(items, item{p, n, text})
	}
	cancel() // stops rg when we stopped reading
	cmd.Wait()
	truncated = truncated || ctx.Err() == context.DeadlineExceeded && r.Context().Err() == nil
	writeJSON(w, 200, map[string]any{"items": items, "truncated": truncated, "ms": math.Round(float64(time.Since(t0).Microseconds())/10) / 100})
}

// ---- GitHub: the owner's repos, and cloning ----

type ghRepo struct {
	Name    string `json:"name"`
	Private bool   `json:"private"`
	Lang    string `json:"lang"`
	Updated string `json:"updated"`
	Desc    string `json:"desc"`
	Here    string `json:"here"` // a local clone of it
}

var ghRepos struct {
	sync.Mutex
	at   time.Time
	list []ghRepo
}

func (s *server) fsRepos(w http.ResponseWriter, r *http.Request) {
	ghRepos.Lock()
	list, fresh := ghRepos.list, time.Since(ghRepos.at) < 5*time.Minute
	ghRepos.Unlock()
	if !fresh {
		var err error
		if list, err = ghRepoList(findBin("gh", filepath.Join(s.cfg.home, ".local/bin/gh"))); err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		ghRepos.Lock()
		ghRepos.list, ghRepos.at = list, time.Now()
		ghRepos.Unlock()
	}
	here := localClones(s.cfg.home)
	out := slices.Clone(list)
	for i := range out {
		out[i].Here = here[strings.ToLower(out[i].Name)]
	}
	writeJSON(w, 200, map[string]any{"repos": out})
}

// ghRepoList is the user's repos and their orgs' repos, most recently updated first.
func ghRepoList(gh string) ([]ghRepo, error) {
	if gh == "" {
		return nil, errors.New("the GitHub CLI (gh) isn't installed")
	}
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, gh, args...)
		cmd.Env = cleanEnv(os.Environ())
		return cmd.Output()
	}
	orgs, err := run("api", "user/orgs", "--jq", ".[].login")
	if err != nil {
		return nil, errors.New("gh isn't logged in (run: gh auth login)")
	}
	owners := append([]string{""}, strings.Fields(string(orgs))...) // "": the user's own
	lists, errs := make([][]ghRepo, len(owners)), make([]error, len(owners))
	var wg sync.WaitGroup
	for i, o := range owners {
		wg.Go(func() {
			args := []string{"repo", "list"}
			if o != "" {
				args = append(args, o)
			}
			out, err := run(append(args, "--json", "nameWithOwner,isPrivate,primaryLanguage,updatedAt,description", "--limit", "200")...)
			var raw []struct {
				NameWithOwner, UpdatedAt, Description string
				IsPrivate                             bool
				PrimaryLanguage                       struct{ Name string }
			}
			if err == nil {
				err = json.Unmarshal(out, &raw)
			}
			for _, x := range raw {
				lists[i] = append(lists[i], ghRepo{x.NameWithOwner, x.IsPrivate, x.PrimaryLanguage.Name, x.UpdatedAt, x.Description, ""})
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if errs[0] != nil {
		return nil, fmt.Errorf("gh repo list: %v", errs[0])
	}
	all := slices.Concat(lists...)
	slices.SortStableFunc(all, func(a, b ghRepo) int { return strings.Compare(b.Updated, a.Updated) }) // RFC 3339 in UTC sorts as text
	return all, nil
}

var ghRemoteRe = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// localClones maps "owner/name" (lowercase) to its clone under ~/repos or ~/projects, by the origin remote.
func localClones(home string) map[string]string {
	m := map[string]string{}
	for _, base := range []string{"repos", "projects"} {
		for _, d := range subdirs(filepath.Join(home, base)) {
			b, _ := os.ReadFile(filepath.Join(d, ".git/config"))
			origin := false
			for _, l := range strings.Split(string(b), "\n") {
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "[") {
					origin = l == `[remote "origin"]`
				} else if k, v, _ := strings.Cut(l, "="); origin && strings.TrimSpace(k) == "url" {
					if g := ghRemoteRe.FindStringSubmatch(strings.TrimSpace(v)); g != nil && m[strings.ToLower(g[1])] == "" {
						m[strings.ToLower(g[1])] = d
					}
				}
			}
		}
	}
	return m
}

var fsRemoteRe = regexp.MustCompile(`^(https://|ssh://|[\w.-]+@[\w.-]+:)`) // https, ssh, or scp-like git@host:path

var ghShortRe = regexp.MustCompile(`^(?:(?:https://)?(?:www\.)?github\.com/)?([A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// fsClone clones a repo into dir/name and streams git's progress as NDJSON: {"p": "..."} lines, then
// {"ok": true, "path"} or {"error"}. GitHub repos go through gh, which has the owner's auth for private ones.
func (s *server) fsClone(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL, Dir, Name, Branch string
		Depth                  int
	}
	if readJSONN(r, &b, 8<<10) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	bad := func(v string) bool {
		return strings.HasPrefix(v, "-") || strings.IndexFunc(v, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0
	}
	if b.URL == "" || bad(b.URL) || bad(b.Branch) {
		writeJSON(w, 400, map[string]any{"error": "not a repository URL (or branch)"})
		return
	}
	if !fsRemoteRe.MatchString(b.URL) && !ghShortRe.MatchString(b.URL) { // a local repo: only one inside the roots, like all else here
		p, err := s.fsPath(b.URL, true)
		if err != nil {
			fsFail(w, err)
			return
		}
		b.URL = p
	}
	if b.Name == "" {
		u := strings.TrimSuffix(strings.TrimRight(b.URL, "/"), ".git")
		b.Name = u[strings.LastIndexAny(u, "/:")+1:]
	}
	if !fsName(b.Name) {
		writeJSON(w, 400, map[string]any{"error": "the folder name " + errFsName.Error()})
		return
	}
	dir, err := s.fsPath(b.Dir, true)
	dest := filepath.Join(dir, b.Name)
	if err == nil {
		err = os.Mkdir(dest, 0o755) // ours, so a failed clone can remove it
	}
	if err != nil {
		fsFail(w, err)
		return
	}
	var flags []string
	if b.Depth == 1 {
		flags = append(flags, "--depth", "1")
	}
	if b.Branch != "" {
		flags = append(flags, "--branch", b.Branch)
	}
	args := []string{"clone", "--progress"}
	if m := ghShortRe.FindStringSubmatch(b.URL); m != nil {
		// GitHub over HTTPS with gh's token, whatever protocol gh prefers: `gh repo clone` goes by SSH when it's set
		// to, and this box's key needn't be on GitHub
		b.URL = "https://github.com/" + m[1] + ".git"
		if gh := findBin("gh", filepath.Join(s.cfg.home, ".local/bin/gh")); gh != "" {
			args = append([]string{"-c", "credential.helper=", "-c", "credential.helper=!" + gh + " auth git-credential"}, args...)
		}
	}
	cmd := exec.CommandContext(r.Context(), "git", append(append(args, flags...), "--", b.URL, dest)...)
	cmd.Env = append(cleanEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // a closed page stops gh and the git under it
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stderr, err := cmd.StderrPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		os.RemoveAll(dest)
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc, enc := http.NewResponseController(w), json.NewEncoder(w)
	send := func(v any) { enc.Encode(v); rc.Flush() }
	sc := bufio.NewScanner(stderr)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) { // git redraws its progress with \r
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	last, sent := "", time.Time{}
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = trunc(l, 300)
			if time.Since(sent) >= 100*time.Millisecond {
				send(map[string]any{"p": last})
				sent = time.Now()
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		os.RemoveAll(dest)
		s.audit.add("fs-clone-fail", trunc(b.URL, 200)+": "+trunc(last, 200), clientIP(r))
		send(map[string]any{"error": cmp.Or(last, err.Error())})
		return
	}
	s.audit.add("fs-clone", trunc(b.URL+" → "+tilde(dest), 300), clientIP(r))
	s.fsx.note(dest)
	send(map[string]any{"ok": true, "path": dest})
}
