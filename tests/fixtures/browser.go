//go:build browserintegration

package fixtures

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type Account struct{ Profile, Username, Password, Label string }
type BrowserSite struct {
	URL                       string
	Accounts                  [2]Account
	Posts, Redirects, Foreign atomic.Int32
}

// NewBrowser owns HTTP fixtures only; production sessions own browser processes.
func NewBrowser(t *testing.T) *BrowserSite {
	t.Helper()
	site := &BrowserSite{Accounts: [2]Account{
		{"alpha", "alpha-private-user-427ac1", "alpha-private-password-427ac1", "Account Alpha verified"},
		{"beta", "beta-private-user-968bd5", "beta-private-password-968bd5", "Account Beta verified"},
	}}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		site.Foreign.Add(1)
		_, _ = io.WriteString(w, "Foreign document")
	}))
	t.Cleanup(foreign.Close)
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<form method="post" action="/session"><label>Username<input name="username" autocomplete="username"></label><label>Password<input name="password" type="password" autocomplete="current-password"></label><button type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		site.Posts.Add(1)
		if r.ParseForm() == nil {
			for _, account := range site.Accounts {
				if r.Form.Get("username") == account.Username && r.Form.Get("password") == account.Password {
					http.SetCookie(w, &http.Cookie{Name: "account", Value: account.Profile, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
					http.Redirect(w, r, "/account", http.StatusSeeOther)
					return
				}
			}
		}
		http.Error(w, "Invalid fixture credentials", http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		cookie, err := r.Cookie("account")
		if err == nil {
			for _, account := range site.Accounts {
				if account.Profile == cookie.Value {
					_, _ = fmt.Fprintf(w, `<title>%s</title><h1>%s</h1><p>Credential echo %s %s</p><label>Search details<input id="query" aria-label="Search details"></label><button id="find">Load details</button><p id="result">Search not started</p><input type="password" value="hidden-form-value"><p hidden>hidden-page-value</p><script>document.querySelector('#find').onclick=()=>{if(document.querySelector('#query').value==='study')document.querySelector('#result').textContent='The study measured 42 samples.';};</script>`, html.EscapeString(account.Label), html.EscapeString(account.Label), html.EscapeString(account.Username), html.EscapeString(account.Password))
					return
				}
			}
		}
		_, _ = io.WriteString(w, `<h1>Signed out</h1>`)
	})
	mux.HandleFunc("GET /details", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<title>Study details</title><p>The study measured 42 samples.</p><a href="/redirect">Follow redirect</a>`)
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		site.Redirects.Add(1)
		http.Redirect(w, r, foreignURL+"/private", http.StatusFound)
	})
	mux.HandleFunc("GET /challenge", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<h1>Account Alpha verified</h1><label>Authentication code<input autocomplete="one-time-code" name="otp"></label>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	site.URL = server.URL
	return site
}
