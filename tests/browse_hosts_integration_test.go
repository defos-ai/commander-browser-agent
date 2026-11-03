//go:build browserintegration

package browser_test

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
)

func TestBrowseWildcardHostsAllowCrossHostDocuments(t *testing.T) {
	_ = browserSessionOptions(t)
	for _, test := range []struct {
		name, link string
		hosts      [][]string
	}{
		{name: "omitted_typed_navigation"},
		{name: "empty_link_click", link: "Read external study", hosts: [][]string{{}}},
		{name: "wildcard_redirect", link: "Follow study redirect", hosts: [][]string{{"*"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			site, destination, hits, redirects := fixtures.NewBrowseHostsSite(t)
			originURL, err := url.Parse(site.URL)
			if err != nil {
				t.Fatal(err)
			}
			destinationURL, err := url.Parse(destination)
			if err != nil || originURL.Hostname() == destinationURL.Hostname() {
				t.Fatal("cross-host fixture does not use distinct hostnames")
			}
			const key = "fixture-wildcard-model-key-632cf150"
			model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
				if !slices.Equal(turn.AllowedHosts, []string{"*"}) {
					t.Errorf("model received scope %v, want sole wildcard", turn.AllowedHosts)
				}
				if len(turn.Evidence) > 0 {
					return intelligence.Command{Action: "finish", Summary: "Found the external study sample count.", Findings: []intelligence.Finding{{Text: "The study measured 42 samples.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
				}
				if turn.Observation.URL == destination && strings.Contains(turn.Observation.Text, "42 samples") {
					return intelligence.Command{Action: "extract"}
				}
				if test.link == "" {
					return intelligence.Command{Action: "navigate", URL: destination}
				}
				for _, target := range turn.Observation.Targets {
					if target.Label == test.link {
						return intelligence.Command{Action: "click", Target: target.ID}
					}
				}
				t.Error("model did not observe the external study navigation target")
				return intelligence.Command{Action: "finish", Summary: "Destination unavailable."}
			})
			framework := startBrowseFramework(t, key)
			peer := framework.peer
			binding := newBrowserSessionBinding(framework.host.ID())
			command := publishBrowse(t, peer.bus, binding, site.URL, 5, test.hosts...)
			answerBrowseQuestion(t, peer, command, model, "browser_model_provider", "More providers", key, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, false)
			answerBrowseQuestion(t, peer, command, model, "browser_model_provider_more", "Other (OpenAI-compatible)", key, []string{"xAI", "DeepSeek", "Other (OpenAI-compatible)"}, false)
			answerBrowseQuestion(t, peer, command, model, "browser_model_name", "fixture-model", key, nil, false)
			answerBrowseQuestion(t, peer, command, model, "browser_model_url", model.URL, key, nil, false)
			deliverBrowseModelSecret(t, peer, model, actions.ModelSecretName(model.URL), key)
			output := receiveBrowse(t, peer, command, key)
			var findings actions.BrowseOutput
			if err := json.Unmarshal(output.Output, &findings); err != nil {
				t.Fatal(err)
			}
			if findings.Status != "completed" || findings.FinalURL != destination || len(findings.Findings) != 1 || len(findings.Evidence) != 1 || findings.Evidence[0].URL != destination || !strings.Contains(findings.Evidence[0].Text, "42 samples") || hits.Load() == 0 {
				t.Fatalf("wildcard scope did not yield grounded findings across hostnames: %+v", findings)
			}
			if test.name == "wildcard_redirect" && redirects.Load() == 0 {
				t.Fatal("wildcard probe did not follow the HTTP redirect")
			}
			if model.Calls() != 3 || findings.Usage.ModelCalls != 3 {
				t.Fatalf("cross-host workflow was replayed or incomplete: model_calls=%d usage=%+v", model.Calls(), findings.Usage)
			}
			checkBrowseArtifacts(t, framework.files, output, key)
		})
	}
}
