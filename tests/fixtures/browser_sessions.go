//go:build browserintegration

package fixtures

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// NewBrowserCleanupFailureExecutable makes the runtime's private profile parent
// nonwritable before startup fails. Only the production manager launches it.
func NewBrowserCleanupFailureExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fail-browser-startup")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nchmod 500 \"$TMPDIR\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// NewBrowserSessionSite uses dummy credentials and an HttpOnly cookie; account
// status comes from the server's cookie check, rather than page-local state.
// Browser process and connection lifecycle belong to the production manager.
func NewBrowserSessionSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(browserSessionLogin))
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("username") != "fixture-user" || r.Form.Get("password") != "fixture-password" {
			http.Error(w, "Invalid fixture credentials", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "browser_session", Value: "fixture-auth", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		status := "signed out"
		if cookie, err := r.Cookie("browser_session"); err == nil && cookie.Value == "fixture-auth" {
			status = "signed in"
		}
		_, _ = w.Write([]byte("<!doctype html><html><head><title>Fixture account</title></head><body><h1 id=account>" + status + "</h1></body></html>"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

const browserSessionLogin = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Fixture login</title></head><body>
<form id="login" method="post" action="/session">
  <label>Username <input id="username" name="username"></label>
  <label>Password <input id="password" name="password" type="password"></label>
  <button type="submit">Sign in</button>
</form><p id="status" data-authenticated="false">Signed out</p>
<script>
document.querySelector("#login").addEventListener("submit", async (event) => {
  event.preventDefault();
  const response = await fetch(event.target.action, {
    method: "POST", body: new URLSearchParams(new FormData(event.target))
  });
  document.querySelector("#status").dataset.authenticated = String(response.ok);
});
</script></body></html>`
