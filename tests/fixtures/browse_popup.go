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

// NewBrowsePopupSite exposes a script-created popup and a known separate-target
// link. Both point at a distinct hostname; counters observe actual HTTP traffic.
func NewBrowsePopupSite(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	foreignHits, separateTargetHits := &atomic.Int32{}, &atomic.Int32{}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		_, _ = io.WriteString(w, "foreign-popup-document-canary")
	}))
	t.Cleanup(foreign.Close)
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /window", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><title>Popup fixture</title><button onclick="window.open('%s/private')">Open popup</button>`, foreignURL)
	})
	mux.HandleFunc("GET /blank", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>Separate target fixture</title><a href="/blank-redirect" target="_blank">Open popup</a>`)
	})
	mux.HandleFunc("GET /blank-redirect", func(w http.ResponseWriter, r *http.Request) {
		separateTargetHits.Add(1)
		http.Redirect(w, r, foreignURL+"/private", http.StatusFound)
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	return origin, foreignHits, separateTargetHits
}
