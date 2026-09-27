package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

const (
	maxText     = 1 << 20 // larger files are not previewed as text
	maxHits     = 200
	maxZipBytes = 2 << 30
	maxZipFiles = 20000
)

var (
	mdExt        = set(".md", ".markdown")
	htmlExt      = set(".html", ".htm")
	imageExt     = set(".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp", ".ico")
	storedExt    = set(".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".zip", ".gz", ".tgz", ".xz", ".7z", ".mp3", ".mp4", ".mov", ".pptx", ".docx", ".xlsx")
	skipInSearch = set("node_modules", "__pycache__", "site-packages", "venv")
	readmeNames  = []string{"README.md", "readme.md", "index.md"}
)

// Content-Security-Policy for raw files that a browser could execute.
// HTML pages may run their own scripts, but in an opaque origin, so they can't
// read the app's API or storage. SVG/XML get no scripting at all.
var rawCSP = map[string]string{
	"text/html":             "sandbox allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox",
	"application/xhtml+xml": "sandbox allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox",
	"image/svg+xml":         "sandbox",
	"text/xml":              "sandbox",
	"application/xml":       "sandbox",
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// Browser serves one folder read-only.
type Browser struct {
	root     string // absolute, symlinks resolved
	download bool
}

func NewBrowser(root string, download bool) (*Browser, error) {
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", root)
	}
	return &Browser{root: abs, download: download}, nil
}

// resolve maps a slash-separated path relative to the root to an absolute path.
// Hidden segments (including "..") are rejected, and the resolved target, after
// following symlinks, must stay inside the root.
func (b *Browser) resolve(rel string) (abs, clean string, err error) {
	var parts []string
	for _, s := range strings.Split(rel, "/") {
		if s == "" || s == "." {
			continue
		}
		if strings.HasPrefix(s, ".") {
			return "", "", errNotFound
		}
		parts = append(parts, s)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(append([]string{b.root}, parts...)...))
	if err != nil {
		return "", "", errNotFound
	}
	if real != b.root && !strings.HasPrefix(real, b.root+string(filepath.Separator)) {
		return "", "", errOutside
	}
	return real, strings.Join(parts, "/"), nil
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func escapePath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func readme(dir string) (string, os.FileInfo) {
	for _, name := range readmeNames {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			return name, fi
		}
	}
	return "", nil
}

type entry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

func (b *Browser) List(req *http.Request) (any, error) {
	abs, clean, err := b.resolve(req.URL.Query().Get("p"))
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, "Not a readable folder"}
	}
	entries := []entry{}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		isDir := de.IsDir()
		if de.Type()&fs.ModeSymlink != 0 {
			fi, err := os.Stat(filepath.Join(abs, de.Name()))
			if err != nil {
				continue
			}
			isDir = fi.IsDir()
		}
		entries = append(entries, entry{de.Name(), isDir})
	}
	slices.SortFunc(entries, func(x, y entry) int {
		if x.Dir != y.Dir {
			if x.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name))
	})
	return map[string]any{
		"root":     filepath.Base(b.root),
		"path":     clean,
		"download": b.download,
		"entries":  entries,
	}, nil
}

// View describes how the client should show a path. Markdown, HTML, images and
// PDFs are loaded by the client from /mk/ or /raw/; text is highlighted here.
func (b *Browser) View(req *http.Request) (any, error) {
	abs, clean, err := b.resolve(req.URL.Query().Get("p"))
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, errNotFound
	}
	if fi.IsDir() {
		if name, rfi := readme(abs); name != "" {
			return map[string]any{"kind": "md", "dir": true, "path": joinRel(clean, name), "mtime": rfi.ModTime().UnixMilli()}, nil
		}
		return map[string]any{"kind": "dir", "dir": true, "path": clean}, nil
	}
	v := map[string]any{"path": clean, "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()}
	ext := strings.ToLower(filepath.Ext(abs))
	switch {
	case mdExt[ext]:
		v["kind"] = "md"
	case htmlExt[ext]:
		v["kind"] = "html"
	case imageExt[ext]:
		v["kind"] = "image"
	case ext == ".pdf":
		v["kind"] = "pdf"
	case fi.Size() > maxText:
		v["kind"] = "binary"
	default:
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data) {
			v["kind"] = "binary"
		} else {
			v["kind"] = "text"
			v["html"] = highlight(filepath.Base(abs), string(data))
		}
	}
	return v, nil
}

// Stat lets the client poll for changes to the file it is showing.
func (b *Browser) Stat(req *http.Request) (any, error) {
	abs, _, err := b.resolve(req.URL.Query().Get("p"))
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, errNotFound
	}
	return map[string]any{"mtime": fi.ModTime().UnixMilli(), "size": fi.Size()}, nil
}

type hit struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
}

// Find matches file and folder names (case-insensitive substring).
func (b *Browser) Find(req *http.Request) (any, error) {
	q := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("q")))
	hits := []hit{}
	if utf8.RuneCountInString(q) < 2 {
		return map[string]any{"hits": hits, "truncated": false}, nil
	}
	deadline := time.Now().Add(3 * time.Second)
	truncated := false
	filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == b.root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || (d.IsDir() && skipInSearch[name]) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.Contains(strings.ToLower(name), q) {
			rel, _ := filepath.Rel(b.root, p)
			hits = append(hits, hit{filepath.ToSlash(rel), d.IsDir()})
		}
		if len(hits) >= maxHits || time.Now().After(deadline) {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	return map[string]any{"hits": hits, "truncated": truncated}, nil
}

// Raw serves file bytes (with Range support). ?download=1 sends it as an attachment.
func (b *Browser) Raw(w http.ResponseWriter, req *http.Request) {
	b.serveFile(w, req, strings.TrimPrefix(req.URL.Path, "/raw/"))
}

func (b *Browser) serveFile(w http.ResponseWriter, req *http.Request, rel string) {
	abs, _, err := b.resolve(rel)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeErr(w, errNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeErr(w, errNotFound)
		return
	}
	name := filepath.Base(abs)
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if base, _, _ := mime.ParseMediaType(ctype); rawCSP[base] != "" {
		w.Header().Set("Content-Security-Policy", rawCSP[base])
	}
	if req.URL.Query().Has("download") {
		if !b.download {
			writeErr(w, errNoDL)
			return
		}
		w.Header().Set("Content-Disposition", attachment(name))
	}
	http.ServeContent(w, req, name, fi.ModTime(), f)
}

// Markdown renders .md files through MkDocs; other paths under /mk/ (images and
// files referenced relatively from a page) are served raw.
func (b *Browser) Markdown(r *Renderer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rel := strings.TrimPrefix(req.URL.Path, "/mk/")
		abs, clean, err := b.resolve(rel)
		if err != nil {
			writeErr(w, err)
			return
		}
		fi, err := os.Stat(abs)
		if err != nil {
			writeErr(w, errNotFound)
			return
		}
		if fi.IsDir() {
			if name, _ := readme(abs); name != "" {
				http.Redirect(w, req, "/mk/"+escapePath(joinRel(clean, name)), http.StatusFound)
				return
			}
			writeErr(w, errNotFound)
			return
		}
		if !mdExt[strings.ToLower(filepath.Ext(abs))] {
			b.serveFile(w, req, rel)
			return
		}
		page, err := r.Render(abs, fi)
		if err != nil {
			http.Error(w, "Markdown render failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	}
}

// Zip downloads

type zipFile struct {
	abs, rel string
	fi       os.FileInfo
}

type zipPlan struct {
	name  string // download file name
	base  string // common parent folder, stripped from entry names
	files []zipFile
	bytes int64
}

var errTooBig = &httpError{http.StatusRequestEntityTooLarge,
	fmt.Sprintf("Selection is too large to zip (limit %d files / %d GiB)", maxZipFiles, maxZipBytes>>30)}

func (b *Browser) planZip(req *http.Request) (*zipPlan, error) {
	if !b.download {
		return nil, errNoDL
	}
	sel := req.URL.Query()["p"]
	if len(sel) == 0 {
		return nil, &httpError{http.StatusBadRequest, "Nothing selected"}
	}
	plan := &zipPlan{}
	seen := map[string]bool{}
	add := func(abs, rel string, fi os.FileInfo) error {
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		plan.files = append(plan.files, zipFile{abs, rel, fi})
		plan.bytes += fi.Size()
		if len(plan.files) > maxZipFiles || plan.bytes > maxZipBytes {
			return errTooBig
		}
		return nil
	}
	var cleans []string
	for _, p := range sel {
		abs, clean, err := b.resolve(p)
		if err != nil {
			return nil, err
		}
		cleans = append(cleans, clean)
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, errNotFound
		}
		if !fi.IsDir() {
			if err := add(abs, clean, fi); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == abs {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() { // folders are walked; symlinks are skipped
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(abs, p)
			return add(p, joinRel(clean, filepath.ToSlash(rel)), info)
		})
		if err != nil {
			return nil, err
		}
	}
	plan.base = commonParent(cleans)
	switch {
	case len(sel) == 1 && cleans[0] != "":
		plan.name = path.Base(cleans[0]) + ".zip"
	case plan.base != "":
		plan.name = path.Base(plan.base) + ".zip"
	default:
		plan.name = filepath.Base(b.root) + ".zip"
	}
	return plan, nil
}

// commonParent returns the deepest folder containing every selected path.
func commonParent(paths []string) string {
	var common []string
	for i, p := range paths {
		dir := path.Dir(p)
		var parts []string
		if dir != "." {
			parts = strings.Split(dir, "/")
		}
		if i == 0 {
			common = parts
			continue
		}
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

// ZipInfo reports what a zip download would contain, so the client can show
// errors (too large, not found) before starting the download.
func (b *Browser) ZipInfo(req *http.Request) (any, error) {
	plan, err := b.planZip(req)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": plan.name, "files": len(plan.files), "bytes": plan.bytes}, nil
}

func (b *Browser) Zip(w http.ResponseWriter, req *http.Request) {
	plan, err := b.planZip(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(plan.name))
	zw := zip.NewWriter(w)
	prefix := ""
	if plan.base != "" {
		prefix = plan.base + "/"
	}
	for _, f := range plan.files {
		hdr := &zip.FileHeader{
			Name:     strings.TrimPrefix(f.rel, prefix),
			Method:   zip.Deflate,
			Modified: f.fi.ModTime(),
		}
		if storedExt[strings.ToLower(filepath.Ext(f.abs))] {
			hdr.Method = zip.Store
		}
		if err := copyInto(zw, hdr, f.abs); err != nil {
			log.Printf("zip %s: %v", plan.name, err)
			return // client sees a truncated download
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("zip %s: %v", plan.name, err)
	}
}

func copyInto(zw *zip.Writer, hdr *zip.FileHeader, abs string) error {
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

func attachment(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

// Code highlighting

var codeFormatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.TabWidth(4))

func highlight(name, text string) string {
	lexer := lexers.Match(name)
	if lexer == nil {
		lexer = lexers.Analyse(text)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	var buf bytes.Buffer
	it, err := chroma.Coalesce(lexer).Tokenise(nil, text)
	if err == nil {
		err = codeFormatter.Format(&buf, styles.Get("github"), it)
	}
	if err != nil {
		return "<pre>" + html.EscapeString(text) + "</pre>"
	}
	return buf.String()
}

func ChromaCSS() []byte {
	var buf bytes.Buffer
	if err := codeFormatter.WriteCSS(&buf, styles.Get("github")); err != nil {
		log.Print(err)
	}
	buf.WriteString("\n@media (prefers-color-scheme: dark) {\n")
	if err := codeFormatter.WriteCSS(&buf, styles.Get("github-dark")); err != nil {
		log.Print(err)
	}
	buf.WriteString("}\n")
	return buf.Bytes()
}

