//go:build browserintegration

package fixtures

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// LoginSite owns HTTP fixtures only; the production manager owns every browser.
type LoginSite struct {
	URL         string
	Username    string
	Password    string
	Submissions atomic.Int32
	ForeignHits atomic.Int32
}

func NewLoginSite(t *testing.T) *LoginSite {
	t.Helper()
	site := &LoginSite{Username: "fixture-login-user-f04a214e", Password: "fixture-login-password-6ecb519d"}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		site.ForeignHits.Add(1)
		_, _ = io.WriteString(w, "Foreign login destination")
	}))
	t.Cleanup(foreign.Close)
	foreignAddress := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
	mux := http.NewServeMux()
	for _, path := range []string{"/login", "/foreign", "/override", "/challenge", "/challenge-after", "/drop"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			action, override, challenge := "/session", "", ""
			switch r.URL.Path {
			case "/foreign":
				action = foreignAddress + "/session"
			case "/override":
				override = ` formaction="` + foreignAddress + `/session"`
			case "/challenge":
				challenge = `<label>Authentication code <input name="otp" autocomplete="one-time-code"></label>`
			case "/challenge-after":
				action = "/session-challenge"
			case "/drop":
				action = "/drop-session"
			}
			_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>Managed login fixture</title></head><body><form method="post" action="%s"><label>Username <input name="username" autocomplete="username"></label><label>Password <input name="password" type="password" autocomplete="current-password"></label>%s<button type="submit"%s>Sign in</button></form></body></html>`, html.EscapeString(action), challenge, override)
		})
	}
	submit := func(w http.ResponseWriter, r *http.Request) {
		site.Submissions.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("username") != site.Username || r.Form.Get("password") != site.Password {
			http.Error(w, "Rejected fixture credentials", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/session-challenge" {
			http.Redirect(w, r, "/otp", http.StatusSeeOther)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "managed_login", Value: "fixture-authenticated", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	}
	mux.HandleFunc("POST /session", submit)
	mux.HandleFunc("POST /session-challenge", submit)
	mux.HandleFunc("GET /otp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Authentication challenge</title></head><body><h1>Complete verification</h1><form method="post"><label>Authentication code <input name="otp" autocomplete="one-time-code"></label><button type="submit">Verify</button></form></body></html>`)
	})
	verificationChallenge := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Account step-up</title></head><body><h1>Account verified</h1><form method="post"><label>Authentication code <input name="otp" autocomplete="one-time-code"></label><button type="submit">Verify</button></form></body></html>`)
	}
	mux.HandleFunc("GET /account-challenge", func(w http.ResponseWriter, _ *http.Request) {
		verificationChallenge(w)
	})
	mux.HandleFunc("GET /account-stepup", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("managed_login"); err != nil || cookie.Value != "fixture-authenticated" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><html><body><h1>Signed out</h1></body></html>`)
			return
		}
		verificationChallenge(w)
	})
	mux.HandleFunc("POST /drop-session", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("username") != site.Username || r.Form.Get("password") != site.Password {
			http.Error(w, "Rejected fixture credentials", http.StatusUnauthorized)
			return
		}
		site.Submissions.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cookie, err := r.Cookie("managed_login"); err != nil || cookie.Value != "fixture-authenticated" {
			_, _ = io.WriteString(w, `<!doctype html><html><head><title>Fixture account</title></head><body><h1>Signed out</h1></body></html>`)
			return
		}
		// Each excerpt boundary cuts after 16 distinctive credential bytes.
		// Redacting only after truncation would miss these partial values.
		title := strings.Repeat("T", 256-16) + site.Username
		label := strings.Repeat("L", 160-16) + site.Password
		body := "Account verified. The private study measured 42 samples. Authenticated account " + site.Username + " password echo " + site.Password + ". "
		body += strings.Repeat("E", 4000-16-len(body)) + site.Username
		body += strings.Repeat("O", 10000-16-len(body)) + site.Password
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title></head><body><main>%s</main><a aria-label="%s" href="/echo/%s/%s">Account details %s %s</a></body></html>`, html.EscapeString(title), html.EscapeString(body), html.EscapeString(label), url.PathEscape(site.Username), url.PathEscape(site.Password), html.EscapeString(site.Username), html.EscapeString(site.Password))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	site.URL = server.URL
	return site
}
