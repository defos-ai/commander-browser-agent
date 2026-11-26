//go:build integration

package fixtures

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

type publicReport struct {
	text   string
	visits atomic.Int32
}

var reportIdentifier = regexp.MustCompile(`\bREPORT-[A-Z2-7]+\b`)

func (site *Mission) servePublicReports(mux *http.ServeMux) {
	mux.HandleFunc("GET /reports", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<a href="/reports/north">North</a> <a href="/reports/south">South</a>`)
	})
	site.reports = make(map[string]*publicReport)
	for _, choice := range []string{"North", "South"} {
		report := &publicReport{text: "REPORT-" + rand.Text()}
		site.reports[choice] = report
		mux.HandleFunc("GET /reports/"+strings.ToLower(choice), func(w http.ResponseWriter, _ *http.Request) {
			report.visits.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<p id="report-id">%s</p>`, report.text)
		})
	}
}

func (site *Mission) PublicReportText(choice string) string { return site.reports[choice].text }

func (site *Mission) AssertPublicVisits(t *testing.T, choice string) {
	t.Helper()
	if site.reports[choice] == nil {
		t.Fatalf("unknown report choice %q", choice)
	}
	for name, report := range site.reports {
		if got := report.visits.Load(); (got > 0) != (name == choice) {
			t.Errorf("report %q received %d visits after choosing %q", name, got, choice)
		}
	}
}

func (site *Mission) navigateToAnswer(content json.RawMessage) (string, bool) {
	var reply struct {
		Outcome   string
		Responses []struct {
			Status string
			Answer string `json:"user_response"`
		}
	}
	var text string
	if json.Unmarshal(content, &text) != nil || json.Unmarshal([]byte(text), &reply) != nil ||
		reply.Outcome != "submitted" || len(reply.Responses) != 1 {
		return "", false
	}
	response := reply.Responses[0]
	if response.Status != "answered" || site.reports[response.Answer] == nil {
		return "", false
	}
	return fmt.Sprintf(`import time
url = %q
nav = cdp('Page.navigate', url=url, _response_timeout=5)
assert not nav.get('errorText') and nav.get('frameId') and nav.get('loaderId')
for attempt in range(40):
    frame = cdp('Page.getFrameTree', _response_timeout=5)['frameTree']['frame']
    if frame.get('id') == nav['frameId'] and frame.get('loaderId') == nav['loaderId'] and frame.get('url') == url:
        result = cdp('Runtime.evaluate', expression="document.querySelector('#report-id')?.textContent", returnByValue=True, awaitPromise=False, _response_timeout=5)
        assert 'exceptionDetails' not in result
        identifier = result['result'].get('value')
        if identifier:
            print(identifier)
            break
    time.sleep(0.05)
else:
    raise RuntimeError('report document did not load')`, site.URL+"/reports/"+strings.ToLower(response.Answer)), true
}
