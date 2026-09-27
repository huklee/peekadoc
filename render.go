package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	startTimeout  = 3 * time.Minute // first run may download Python packages
	renderTimeout = time.Minute
	maxCached     = 256
)

var errTimeout = errors.New("renderer timed out")

// Renderer drives one long-lived mkrender.py worker over stdin/stdout.
// Requests are serialized; results are cached by path, mtime and size.
type Renderer struct {
	cmdline []string

	mu     sync.Mutex // guards the worker and serializes requests
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	cacheMu sync.Mutex
	cache   map[string]cachedPage
}

type cachedPage struct {
	mtime time.Time
	size  int64
	html  []byte
}

func NewRenderer(cmdline []string) *Renderer {
	return &Renderer{cmdline: cmdline, cache: map[string]cachedPage{}}
}

// Warm starts the worker ahead of the first request.
func (r *Renderer) Warm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensure(); err != nil {
		log.Printf("renderer: %v", err)
	}
}

func (r *Renderer) Render(abs string, fi os.FileInfo) ([]byte, error) {
	r.cacheMu.Lock()
	c, ok := r.cache[abs]
	r.cacheMu.Unlock()
	if ok && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.html, nil
	}

	r.mu.Lock()
	html, err := r.request(abs)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}

	r.cacheMu.Lock()
	if len(r.cache) >= maxCached {
		clear(r.cache)
	}
	r.cache[abs] = cachedPage{fi.ModTime(), fi.Size(), html}
	r.cacheMu.Unlock()
	return html, nil
}

func (r *Renderer) request(abs string) ([]byte, error) {
	line, _ := json.Marshal(map[string]string{"path": abs})
	line = append(line, '\n')
	// A second attempt covers a worker that died since the last request.
	for attempt := 0; ; attempt++ {
		if err := r.ensure(); err != nil {
			return nil, err
		}
		var resp struct {
			HTML  string `json:"html"`
			Error string `json:"error"`
		}
		_, err := r.stdin.Write(line)
		if err == nil {
			err = r.read(&resp, renderTimeout)
		}
		if err != nil {
			r.stopLocked()
			if attempt == 0 && !errors.Is(err, errTimeout) {
				continue
			}
			return nil, err
		}
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return []byte(resp.HTML), nil
	}
}

func (r *Renderer) ensure() error {
	if r.cmd != nil {
		return nil
	}
	cmd := exec.Command(r.cmdline[0], r.cmdline[1:]...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so Stop can kill uv and python together
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start renderer: %w", err)
	}
	r.cmd, r.stdin, r.stdout = cmd, stdin, bufio.NewReaderSize(stdout, 1<<20)

	var ready struct {
		Ready bool `json:"ready"`
	}
	if err := r.read(&ready, startTimeout); err != nil || !ready.Ready {
		r.stopLocked()
		return fmt.Errorf("renderer did not start: %v", err)
	}
	log.Printf("renderer ready (pid %d)", cmd.Process.Pid)
	return nil
}

func (r *Renderer) read(v any, timeout time.Duration) error {
	rd := r.stdout
	ch := make(chan error, 1)
	go func() {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			ch <- err
			return
		}
		ch <- json.Unmarshal(line, v)
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		return errTimeout
	}
}

func (r *Renderer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *Renderer) stopLocked() {
	if r.cmd == nil {
		return
	}
	cmd := r.cmd
	r.cmd = nil
	r.stdin.Close() // the worker exits on EOF
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}
