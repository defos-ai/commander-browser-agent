//go:build integration

package fixtures

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const (
	Username    = "mission-reader@example.test"
	Password    = "fixture-reader-password"
	OTP         = "835274"
	ModelKey    = "fixture-chat-api-key"
	ModelName   = "gpt-4.1-mini"
	AccountText = "Signed in to the report archive"
)

// Mission serves a two-factor website and its scripted Chat Completions provider
// on the agent's loopback network. The report changes for every test run.
type Mission struct {
	URL, Endpoint, ReportText string
	passwords, codes          atomic.Int32
	validations, observations atomic.Int32
}

func NewMission(t *testing.T) *Mission {
	t.Helper()
	site := &Mission{ReportText: "Archive report " + rand.Text()}
	challenge, session := rand.Text(), rand.Text()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<form method="post" action="/login">
<label>Username <input name="username" autocomplete="username"></label>
<label>Password <input name="password" type="password" autocomplete="current-password"></label>
<button type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("username") != Username || r.PostFormValue("password") != Password {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}
		site.passwords.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "challenge", Value: challenge, Path: "/verify", HttpOnly: true})
		http.Redirect(w, r, "/verify", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /verify", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<form method="post" action="/verify">
<label>Verification code <input name="otp" autocomplete="one-time-code"></label>
<button type="submit">Verify</button></form>`)
	})
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("challenge")
		if err != nil || cookie.Value != challenge || r.PostFormValue("otp") != OTP {
			http.Error(w, "Invalid verification code", http.StatusUnauthorized)
			return
		}
		site.codes.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "session", Value: session, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != session {
			fmt.Fprint(w, "<h1>Signed out</h1>")
			return
		}
		fmt.Fprintf(w, "<h1>%s</h1><p>%s</p><p>Reader %s</p>", AccountText, site.ReportText, Username)
	})
	mux.HandleFunc("POST /v1/chat/completions", site.complete)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	site.URL, site.Endpoint = server.URL, server.URL+"/v1/chat/completions"
	return site
}

func (site *Mission) complete(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+ModelKey {
		http.Error(w, "Invalid model key", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var req struct {
		Model    string
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
		Tools []json.RawMessage
	}
	if err != nil || json.Unmarshal(body, &req) != nil || req.Model != ModelName || len(req.Messages) == 0 {
		http.Error(w, "Invalid completion request", http.StatusBadRequest)
		return
	}
	for _, secret := range []string{Username, Password, OTP, ModelKey} {
		if bytes.Contains(body, []byte(secret)) {
			http.Error(w, "Unexpected credential in model request", http.StatusBadRequest)
			return
		}
	}
	last := req.Messages[len(req.Messages)-1]
	message := map[string]any{"role": "assistant"}
	finish := "stop"
	switch {
	case len(req.Tools) == 0 && len(req.Messages) == 1 && last.Role == "user":
		site.validations.Add(1)
		message["content"] = "OK"
	case last.Role == "user":
		// Inspect the document left open by login on the original browser target.
		args, _ := json.Marshal(map[string]string{"code": `print(cdp('Runtime.evaluate', expression='document.body.textContent', returnByValue=True)['result']['value'])`})
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"id": "read_account", "type": "function",
			"function": map[string]string{"name": "browser_exec", "arguments": string(args)},
		}}
		finish = "tool_calls"
	case last.Role == "tool" && bytes.Contains(last.Content, []byte(site.ReportText)) && bytes.Contains(last.Content, []byte("[REDACTED]")):
		site.observations.Add(1)
		message["content"] = site.ReportText
	default:
		http.Error(w, "Authenticated report observation missing", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "mission-completion", "object": "chat.completion", "created": 1, "model": ModelName,
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
	})
}

func (site *Mission) AssertUsed(t *testing.T) {
	t.Helper()
	for name, count := range map[string]int32{
		"password submission": site.passwords.Load(), "code submission": site.codes.Load(),
		"model validation": site.validations.Load(), "authenticated report observation": site.observations.Load(),
	} {
		if count != 1 {
			t.Errorf("expected one %s, got %d", name, count)
		}
	}
}
