// Command peekadoc is a read-only web file browser for one folder on this machine.
//
// The Go server owns the UI, the folder tree, search, previews and downloads.
// Markdown is rendered with MkDocs Material by a Python worker (mkrender.py, run
// with uv) that peekadoc starts itself and talks to over stdin/stdout.
//
//	go build -o peekadoc . && ./peekadoc -root ~/Work -addr <tailscale-ip>:8000
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed app.html
var appHTML []byte

func main() {
	root := flag.String("root", ".", "folder to serve")
	addr := flag.String("addr", "127.0.0.1:8000", "listen address (use the Tailscale IP to expose it on the tailnet only)")
	script := flag.String("renderer", "mkrender.py", "MkDocs renderer script, run with `uv run`")
	config := flag.String("mkdocs-config", "mkdocs.yml", "MkDocs config used for rendering")
	cache := flag.String("cache", ".cache", "folder for shared MkDocs theme assets")
	noDownload := flag.Bool("no-download", false, "view only: disable download buttons and zip downloads")
	flag.Parse()

	b, err := NewBrowser(*root, !*noDownload)
	if err != nil {
		log.Fatal(err)
	}
	themeDir := mustAbs(filepath.Join(*cache, "theme"))
	if err := os.MkdirAll(themeDir, 0o755); err != nil {
		log.Fatal(err)
	}
	r := NewRenderer([]string{
		"uv", "run", "--quiet", "--script", mustAbs(*script),
		"--config", mustAbs(*config), "--assets", themeDir,
	})
	go r.Warm()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(appHTML)
	})
	chromaCSS := ChromaCSS()
	mux.HandleFunc("GET /chroma.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(chromaCSS)
	})
	mux.HandleFunc("GET /api/list", api(b.List))
	mux.HandleFunc("GET /api/view", api(b.View))
	mux.HandleFunc("GET /api/stat", api(b.Stat))
	mux.HandleFunc("GET /api/find", api(b.Find))
	mux.HandleFunc("GET /api/zipinfo", api(b.ZipInfo))
	mux.HandleFunc("GET /zip", b.Zip)
	mux.HandleFunc("GET /raw/", b.Raw)
	mux.Handle("GET /mk/_theme/", http.StripPrefix("/mk/_theme/", http.FileServer(http.Dir(themeDir))))
	mux.HandleFunc("GET /mk/", b.Markdown(r))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("serving %s read-only on http://%s/ (downloads %s)", b.root, *addr, map[bool]string{true: "on", false: "off"}[b.download])
	err = srv.ListenAndServe()
	r.Stop()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		log.Fatal(err)
	}
	return abs
}

// api adapts a JSON-returning function into a handler.
func api(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		v, err := f(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

var (
	errNotFound = &httpError{http.StatusNotFound, "Not found"}
	errOutside  = &httpError{http.StatusForbidden, "Outside the served folder"}
	errNoDL     = &httpError{http.StatusForbidden, "Downloads are disabled on this server"}
)

func writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		http.Error(w, he.msg, he.code)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
