//go:build hermesintegration

package fixtures

import (
	"context"
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
	Code                      string
	Posts, Redirects, Foreign atomic.Int32
	CodePosts                 atomic.Int32
	CodeInputs                atomic.Int32
	changeCode, codeChanged   chan struct{}
}

func (site *BrowserSite) ChangeCodeDestination(ctx context.Context) error {
	close(site.changeCode)
	select {
	case <-site.codeChanged:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewBrowser owns HTTP fixtures only; production sessions own browser processes.
func NewBrowser(t *testing.T) *BrowserSite {
	t.Helper()
	site := &BrowserSite{Code: "482719", changeCode: make(chan struct{}), codeChanged: make(chan struct{}), Accounts: [2]Account{
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
	codeForm := func(w http.ResponseWriter, target string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<h1>Enter authentication code</h1><form method="post" action="%s"><label>Authentication code<input name="otp" autocomplete="one-time-code"></label><button type="submit">Verify code</button></form>`, target)
	}
	mux.HandleFunc("GET /otp-login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<form method="post" action="/otp-password"><label>Username<input name="username" autocomplete="username"></label><label>Password<input name="password" type="password" autocomplete="current-password"></label><button type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("POST /otp-password", func(w http.ResponseWriter, r *http.Request) {
		site.Posts.Add(1)
		if r.ParseForm() != nil || r.Form.Get("username") != site.Accounts[0].Username || r.Form.Get("password") != site.Accounts[0].Password {
			http.Error(w, "Invalid fixture credentials", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/otp-code", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /otp-code", func(w http.ResponseWriter, _ *http.Request) { codeForm(w, "/otp-verify") })
	mux.HandleFunc("GET /otp-drop-code", func(w http.ResponseWriter, _ *http.Request) { codeForm(w, "/otp-drop") })
	mux.HandleFunc("GET /otp-mutable-code", func(w http.ResponseWriter, _ *http.Request) {
		codeForm(w, "/otp-verify")
		_, _ = io.WriteString(w, `<script>
document.querySelector('input').addEventListener('input',()=>fetch('/otp-input'));
let policyAttempts=0;
function pollPolicy() {
  if(++policyAttempts>20) return;
  fetch('/otp-policy').then(r=>r.text()).then(url=>{
    if(url) {document.querySelector('form').action=url;fetch('/otp-changed');}
    else setTimeout(pollPolicy,500);
  }).catch(()=>setTimeout(pollPolicy,500));
}
pollPolicy();
</script>`)
	})
	mux.HandleFunc("GET /otp-input", func(w http.ResponseWriter, _ *http.Request) {
		site.CodeInputs.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /otp-policy", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		select {
		case <-site.changeCode:
			_, _ = io.WriteString(w, foreignURL+"/private")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /otp-changed", func(w http.ResponseWriter, _ *http.Request) {
		close(site.codeChanged)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /otp-verify", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		site.CodePosts.Add(1)
		if r.Form.Get("otp") != site.Code {
			codeForm(w, "/otp-verify")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "otp-account", Value: "alpha", Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/otp-account", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /otp-drop", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			return
		}
		site.CodePosts.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close() // Native transport may retry; agent must stop uncertain.
		}
	})
	mux.HandleFunc("GET /otp-account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cookie, err := r.Cookie("otp-account"); err == nil && cookie.Value == "alpha" {
			_, _ = fmt.Fprintf(w, `<h1>%s</h1><p>Credential echo %s %s %s</p>`, site.Accounts[0].Label, site.Accounts[0].Username, site.Accounts[0].Password, site.Code)
			return
		}
		_, _ = io.WriteString(w, `<h1>Signed out</h1>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	site.URL = server.URL
	return site
}
