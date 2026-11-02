//go:build browserintegration

package fixtures

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func NewBrowseNavigationSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Study index</title></head><body><h1>Study index</h1><a href="/details">Read study details</a></body></html>`)
	})
	mux.HandleFunc("GET /details", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Study details</title></head><body><h1>Study details</h1><p id="result"></p><script>document.querySelector('#result').textContent='The study measured 42 samples.';</script></body></html>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
