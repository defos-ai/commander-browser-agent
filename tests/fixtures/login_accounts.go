//go:build browserintegration

package fixtures

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

type LoginAccount struct {
	Profile  string
	Username string
	Password string
	Label    string
}

// LoginAccountsSite keeps two identities on one origin. Account responses come
// from the server's distinct HttpOnly cookies, never browser-local account text.
type LoginAccountsSite struct {
	URL         string
	Accounts    [2]LoginAccount
	Submissions atomic.Int32
}

func NewLoginAccountsSite(t *testing.T) *LoginAccountsSite {
	t.Helper()
	site := &LoginAccountsSite{Accounts: [2]LoginAccount{
		{Profile: "alpha", Username: "alpha-private-user-42f9e7", Password: "alpha-private-password-4b39aa", Label: "Account Alpha verified"},
		{Profile: "beta", Username: "beta-private-user-5cb117", Password: "beta-private-password-e11b05", Label: "Account Beta verified"},
	}}
	byCookie := map[string]LoginAccount{}
	for i, account := range site.Accounts {
		byCookie[fmt.Sprintf("fixture-account-token-%d", i)] = account
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Account login</title></head><body><form method="post" action="/session"><label>Username <input name="username" autocomplete="username"></label><label>Password <input name="password" type="password" autocomplete="current-password"></label><button type="submit">Sign in</button></form></body></html>`)
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		site.Submissions.Add(1)
		for token, account := range byCookie {
			if r.Form.Get("username") == account.Username && r.Form.Get("password") == account.Password {
				http.SetCookie(w, &http.Cookie{Name: "account_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
				http.Redirect(w, r, "/account", http.StatusSeeOther)
				return
			}
		}
		http.Error(w, "Invalid fixture credentials", http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		cookie, err := r.Cookie("account_session")
		if err != nil {
			_, _ = io.WriteString(w, `<!doctype html><html><body><h1>Signed out</h1></body></html>`)
			return
		}
		account, ok := byCookie[cookie.Value]
		if !ok {
			http.Error(w, "Unknown fixture account", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title></head><body><h1>%s</h1><p>The private study measured 42 samples.</p><p>Credential echo %s %s</p><a href="/details/%s/%s">Private details %s %s</a></body></html>`, html.EscapeString(account.Label), html.EscapeString(account.Label), html.EscapeString(account.Username), html.EscapeString(account.Password), url.PathEscape(account.Username), url.PathEscape(account.Password), html.EscapeString(account.Username), html.EscapeString(account.Password))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	site.URL = server.URL
	return site
}
