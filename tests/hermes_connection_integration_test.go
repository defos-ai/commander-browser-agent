//go:build hermesintegration

package browser_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/id"
)

// A synchronous page request temporarily prevents Lightpanda from dispatching
// ordinary CDP commands, while the owned browser and connection remain alive.
// The post-batch identity check must wait without replaying the native batch.
func TestHermesNativeBrowseWaitsForSynchronousPageRequest(t *testing.T) {
	site := fixtures.NewSynchronous(t, 2500*time.Millisecond)
	key := "fixture-synchronous-browse-key-" + id.New()
	code := fmt.Sprintf(`import json, time
cdp('Page.navigate', url=%q)
observation = cdp('Runtime.evaluate', expression="(()=>{const style=getComputedStyle(document.getElementById('author-style'));const result={text:document.title+' '+document.body.textContent,display:style.display,visibility:style.visibility};blockSoon(50);return JSON.stringify(result)})()", returnByValue=True)
assert not observation.get('exceptionDetails')
page = json.loads(observation['result']['value'])
print(page['text'])
print('Author stylesheet: display='+page['display']+' visibility='+page['visibility'])
time.sleep(0.2)`, site.URL+"/page")
	var authorCSSObserved atomic.Bool
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("synchronous-page", "browser_exec", map[string]any{"code": code})}}
		case 2:
			authorCSSObserved.Store(request.HasToolText("Author stylesheet: display=none visibility=hidden"))
			if !request.HasToolText("37 samples") {
				t.Error("native browsing lost its observation while waiting for the synchronous page request")
			}
			return fakes.HermesResponse{Text: "The synchronous fixture reports 37 samples."}
		default:
			t.Error("native browsing replayed work after its synchronous page request")
			return fakes.HermesResponse{Text: "Unexpected synchronous fixture continuation."}
		}
	})
	endpoint := model.URL + "/chat/completions"
	h := newDefaultHermesSessionHost(t)
	input := commander.HermesBrowseInput{Objective: "Read the fixture and report its sample count.", URL: site.URL + "/page",
		MaxIterations: 2, MaxTokensPerResponse: 512, TimeoutSeconds: 35}
	task := h.nativeBrowseTask(nil, input)
	binding := h.binding(task)
	command := h.dispatch(task, &binding)
	h.answerModelConfig(command, "other", "gpt-4.1-mini", endpoint)
	h.deliverModelSecret(command, modelSecretName(endpoint), key, 1)
	h.rememberModelConfig(command, "other", "gpt-4.1-mini", endpoint)
	output, ok := h.screenshotResult(command)
	if site.Stylesheets.Load() == 0 {
		t.Fatal("native browsing did not load the same-origin author stylesheet")
	}
	if !authorCSSObserved.Load() {
		t.Fatal("the first native observation did not confirm the author stylesheet's computed display and visibility")
	}
	var result commander.HermesBrowseOutput
	if !ok || json.Unmarshal(output.Output, &result) != nil || result.Validate() != nil || result.Status != "completed" ||
		result.ModelRequests != 2 || !strings.Contains(result.Text, "37 samples") || output.Session == nil || output.Session.Reference != binding.Reference ||
		len(output.Artifacts) != 1 || len(output.Artifacts[0].Files) != 1 || !bytes.Equal(output.Output, h.screenshotDownload(output, "browse.json")) {
		t.Fatal("native browsing did not finish on its original connection after the synchronous page request")
	}
	select {
	case <-site.Started:
	default:
		t.Fatal("the page did not enter its synchronous request")
	}
	if model.ValidationCalls() != 1 || model.Calls() != 2 || site.Visits.Load() != 1 || site.HeldRequests.Load() != 1 {
		t.Fatal("native browsing repeated setup, navigation, or the synchronous page request")
	}
	if h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("the synchronous page request lost the retained connected session")
	}
	assertHermesCheckpointFacts(t, h, command, binding, key, site.URL, code, input.Objective, "37 samples")
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" {
		t.Fatal("the synchronous fixture session did not confirm closure")
	}
}
