package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "__Host-peekadoc-session"

type passwordAuth struct {
	password   [32]byte
	mu         sync.Mutex
	sessions   map[string]bool
	failures   int
	retryAfter time.Time
}

func loadPassword(path string) (string, error) {
	if p := os.Getenv("PEEKADOC_PASSWORD"); p != "" {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		p := randomToken()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, err = fmt.Fprintln(f, p)
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		return p, nil
	}
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return "", fmt.Errorf("password file %s is empty", path)
	}
	return p, nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newPasswordAuth(password string) *passwordAuth {
	return &passwordAuth{password: sha256.Sum256([]byte(password)), sessions: make(map[string]bool)}
}

func (a *passwordAuth) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			u, err := url.Parse(origin)
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && (err != nil || u.Scheme != "https" || u.Host != r.Host)) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if r.URL.Path == "/login" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.Method == "GET" {
				a.loginPage(w, "")
				return
			}
			if r.Method != "POST" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", 400)
				return
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if time.Now().Before(a.retryAfter) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
				a.loginPage(w, "Too many attempts. Try again in a minute.")
				return
			}
			hash := sha256.Sum256([]byte(r.PostForm.Get("password")))
			if subtle.ConstantTimeCompare(hash[:], a.password[:]) != 1 {
				a.failures++
				if a.failures >= 5 {
					a.retryAfter = time.Now().Add(time.Minute)
					a.failures = 0
				}
				w.WriteHeader(401)
				a.loginPage(w, "Incorrect password.")
				return
			}
			if len(a.sessions) >= 4096 {
				http.Error(w, "session limit reached; restart server", 503)
				return
			}
			a.failures = 0
			if old, err := r.Cookie(sessionCookie); err == nil {
				delete(a.sessions, old.Value)
			}
			token := randomToken()
			a.sessions[token] = true
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		a.mu.Lock()
		authenticated := err == nil && a.sessions[cookie.Value]
		if r.URL.Path == "/logout" && r.Method == "POST" && authenticated {
			delete(a.sessions, cookie.Value)
		}
		a.mu.Unlock()
		if r.URL.Path == "/logout" && r.Method == "POST" {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !authenticated {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/raw/") || strings.HasPrefix(r.URL.Path, "/mk/") || r.URL.Path == "/zip" {
				http.Error(w, "login required", 401)
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *passwordAuth) loginPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Sign in · peekadoc</title><style>body{background:#0b0e14;color:#c7ccd4;font:16px system-ui;display:grid;place-items:center;min-height:90vh}main{width:min(320px,85vw)}input,button{box-sizing:border-box;width:100%%;padding:12px;margin-top:12px;border-radius:6px}button{background:#7ee787;cursor:pointer}p{color:#ff7b72}</style><main><h1>peekadoc</h1><form method="post" action="/login"><label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" autofocus required><button>Sign in</button><p role="alert">%s</p></form></main></html>`, message)
}
