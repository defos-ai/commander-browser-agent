//go:build browserintegration

package fixtures

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// NewBrowseHostsSite serves a link and redirect from an IP hostname to localhost.
func NewBrowseHostsSite(t *testing.T) (*httptest.Server, string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	hits, redirects := &atomic.Int32{}, &atomic.Int32{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/details" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>External study details</title></head><body><h1>External study</h1><p>The study measured 42 samples.</p></body></html>`)
	}))
	t.Cleanup(destination.Close)
	address := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1) + "/details"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>Study index</title></head><body><a href="%s">Read external study</a><a href="/redirect">Follow study redirect</a></body></html>`, address)
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		http.Redirect(w, r, address, http.StatusFound)
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	return origin, address, hits, redirects
}
