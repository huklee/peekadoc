package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordSession(t *testing.T) {
	a := newPasswordAuth("test-password")
	h := a.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	request := func(method, path, password string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(url.Values{"password": {password}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/info", "/raw/file.txt", "/mk/file.md", "/zip"} {
		if w := request("GET", path, "", nil, ""); w.Code != 401 {
			t.Fatalf("unprotected %s: %d", path, w.Code)
		}
	}
	if w := request("GET", "/", "", nil, ""); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal("missing login redirect")
	}
	if w := request("POST", "/login", "wrong", nil, ""); w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("wrong password accepted")
	}
	if w := request("POST", "/login", "test-password", nil, "https://evil.test"); w.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
	w := request("POST", "/login", "test-password", nil, "https://example.test")
	if w.Code != 303 {
		t.Fatalf("login: %d", w.Code)
	}
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("bad session cookie: %+v", c)
	}
	if w := request("GET", "/api/info", "", c, ""); w.Code != 204 {
		t.Fatal("session rejected")
	}
	if w := request("POST", "/api/send", "", c, "https://evil.test"); w.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	if w := request("POST", "/logout", "", c, ""); w.Code != 303 {
		t.Fatal("logout failed")
	}
	if w := request("GET", "/api/info", "", c, ""); w.Code != 401 {
		t.Fatal("logged-out cookie accepted")
	}
	c.Value = "forged"
	if w := request("GET", "/api/info", "", c, ""); w.Code != 401 {
		t.Fatal("forged session accepted")
	}
	for i := 0; i < 5; i++ {
		request("POST", "/login", "wrong", nil, "")
	}
	if w := request("POST", "/login", "test-password", nil, ""); w.Code != 429 {
		t.Fatal("login attempts not throttled")
	}
}

func TestPasswordFile(t *testing.T) {
	t.Setenv("PEEKADOC_PASSWORD", "")
	path := filepath.Join(t.TempDir(), ".password")
	first, err := loadPassword(path)
	if err != nil || len(first) != 64 {
		t.Fatalf("generate: %v", err)
	}
	second, err := loadPassword(path)
	if err != nil || second != first {
		t.Fatal("password not persisted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("password file not private")
	}
	os.WriteFile(path, nil, 0600)
	if _, err := loadPassword(path); err == nil {
		t.Fatal("empty password accepted")
	}
}
