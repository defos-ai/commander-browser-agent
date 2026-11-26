//go:build hermesintegration

package fixtures

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SynchronousSite holds an ordinary page request while its synchronous XHR
// occupies the browser's JavaScript stack. It owns HTTP handlers only.
type SynchronousSite struct {
	URL                  string
	Visits, HeldRequests atomic.Int32
	Stylesheets          atomic.Int32
	Started              <-chan struct{}
	release              chan struct{}
	releaseOnce          sync.Once
}

// NewSynchronous releases the held response after hold, or on Release/shutdown.
// A zero hold keeps the response pending until Release or shutdown.
func NewSynchronous(t *testing.T, hold time.Duration) *SynchronousSite {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	var startedOnce sync.Once
	site := &SynchronousSite{Started: started, release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /page", func(w http.ResponseWriter, _ *http.Request) {
		site.Visits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<link rel="stylesheet" href="/fixture.css"><title>Synchronous fixture</title><p>Synchronous fixture has 37 samples.</p><span id="author-style">Author stylesheet probe</span><script>
window.blockSoon = function(delay) {
  setTimeout(function() {
    const request = new XMLHttpRequest();
    request.open('GET', '/held', false);
    request.send();
    document.body.dataset.released = 'true';
  }, delay);
};
</script>`)
	})
	mux.HandleFunc("GET /fixture.css", func(w http.ResponseWriter, _ *http.Request) {
		site.Stylesheets.Add(1)
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, "#author-style { display: none; visibility: hidden; }")
	})
	mux.HandleFunc("GET /held", func(w http.ResponseWriter, r *http.Request) {
		site.HeldRequests.Add(1)
		startedOnce.Do(func() { close(started) })
		var elapsed <-chan time.Time
		if hold > 0 {
			timer := time.NewTimer(hold)
			defer timer.Stop()
			elapsed = timer.C
		}
		select {
		case <-elapsed:
		case <-site.release:
		case <-ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "released")
	})
	server := httptest.NewServer(mux)
	site.URL = server.URL
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	return site
}

func (site *SynchronousSite) Release() {
	site.releaseOnce.Do(func() { close(site.release) })
}
