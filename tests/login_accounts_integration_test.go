//go:build browserintegration

package browser_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestLoginOneAgentRetainsTwoIsolatedAccountsOnTheSameOrigin(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginAccountsSite(t)
	keys := [2]string{"alpha-model-private-key-ef211c", "beta-model-private-key-225df1"}
	var protected []string
	for i, account := range site.Accounts {
		protected = append(protected, account.Username, account.Password, account.Username[:16], account.Password[:16], keys[i])
	}
	var models [2]*fixtures.BrowseModel
	for i, account := range site.Accounts {
		other := site.Accounts[1-i]
		models[i] = fixtures.NewBrowseModel(t, keys[i], func(turn intelligence.Turn) intelligence.Command {
			encoded, err := json.Marshal(turn)
			if err != nil {
				t.Error("could not inspect the account model turn")
			}
			assertAccountValuesAbsent(t, encoded, protected)
			if !strings.Contains(turn.Observation.Text, account.Label) || strings.Contains(turn.Observation.Text, other.Label) {
				t.Error("a browser session observed the other account's authenticated identity")
			}
			if len(turn.Evidence) == 0 {
				return intelligence.Command{Action: "extract"}
			}
			return intelligence.Command{Action: "finish", Summary: account.Label, Findings: []intelligence.Finding{{Text: account.Label, EvidenceIDs: []string{turn.Evidence[0].ID}}}}
		})
	}
	framework := startBrowseFramework(t, keys[0])
	peer := framework.peer
	bindings := [2]sessions.Binding{newBrowserSessionBinding(framework.host.ID()), newBrowserSessionBinding(framework.host.ID())}
	if bindings[0].OwnerAgentID != bindings[1].OwnerAgentID || bindings[0].ID == bindings[1].ID || bindings[0].ScopeID == bindings[1].ScopeID {
		t.Fatal("two-account fixture did not use separate slots under the same agent")
	}
	seenSecrets := map[string]bool{}
	for i, account := range site.Accounts {
		input := loginInput(bindings[i], site.URL, "/login", account.Profile)
		input["expected_text"] = account.Label
		command := publishLogin(t, peer, bindings[i], input)
		username, password := loginSecretNames(site.URL, account.Profile)
		if seenSecrets[username] || seenSecrets[password] {
			t.Fatal("different credential profiles shared managed secret names")
		}
		seenSecrets[username], seenSecrets[password] = true, true
		deliverBrowseModelSecret(t, peer, models[i], username, account.Username)
		deliverBrowseModelSecret(t, peer, models[i], password, account.Password)
		output := receiveBrowse(t, peer, command, protected...)
		var login actions.LoginOutput
		if json.Unmarshal(output.Output, &login) != nil || login.Status != "verified" || !login.Submitted || output.Session == nil || site.Submissions.Load() != int32(i+1) {
			t.Fatal("same-origin account login did not verify exactly one independent submission")
		}
		assertBrowserSession(t, *output.Session, bindings[i].Reference, "connected")
		facts := peer.checkpointFacts(command)
		if len(facts) != 2 {
			t.Fatal("managed login did not retain its start and completion facts")
		}
		for name, data := range facts {
			fact := decodeBrowseFact(t, data)
			if fact.Version != 1 || fact.Action != "login" || fact.Engine != "lightpanda" || fact.Session != bindings[i].Claim() || fact.ResultDelivery != "unconfirmed" || fact.Recovery != "inspect_session_before_explicit_continuation" {
				t.Fatal("login checkpoint lost its exact session claim or promised confirmed delivery")
			}
			switch name {
			case "login_started":
				if fact.Phase != "browser_work_pending" || fact.PendingCommand != "login" || fact.Submitted || fact.Status != "" {
					t.Fatal("login start fact claimed completed or submitted browser work")
				}
			case "login_finished":
				if fact.Phase != "work_prepared" || fact.Status != "verified" || !fact.Submitted {
					t.Fatal("login completion fact did not retain its verified submission outcome")
				}
			default:
				t.Fatal("login retained an unexpected recovery fact")
			}
			assertAccountValuesAbsent(t, data, protected)
			assertCheckpointContentKeysAbsent(t, data)
			for _, privateAccount := range site.Accounts {
				if bytes.Contains(data, []byte(site.URL)) || bytes.Contains(data, []byte(privateAccount.Label)) || bytes.Contains(data, []byte(privateAccount.Profile)) {
					t.Fatal("login checkpoint retained its origin, verification marker or raw credential profile")
				}
			}
		}
		bindings[i].Mode = sessions.Reuse
	}
	// Both logins finish before either browse: account B must not overwrite A.
	for i := range bindings {
		command := publishBrowse(t, peer.bus, bindings[i], site.URL+"/account", 4)
		answerBrowseQuestion(t, peer, command, models[i], "browser_model_provider", "other", keys[i], []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, false)
		answerBrowseQuestion(t, peer, command, models[i], "browser_model_name", "fixture-model", keys[i], nil, false)
		answerBrowseQuestion(t, peer, command, models[i], "browser_model_url", models[i].URL, keys[i], nil, false)
		deliverBrowseModelSecret(t, peer, models[i], modelSetupSecretName(models[i].URL), keys[i])
		assertAccountBrowse(t, framework, command, bindings[i], site.Accounts[i].Label, protected)
		if models[i].Calls() != 2 {
			t.Fatal("account browsing repeated its model workflow")
		}
	}
	closed := peer.control(bindings[0], "close")
	if len(closed.Sessions) != 2 {
		t.Fatal("closing one account discarded another account's inventory")
	}
	for _, item := range closed.Sessions {
		switch item.ID {
		case bindings[0].ID:
			assertBrowserSession(t, item, bindings[0].Reference, "closed")
		case bindings[1].ID:
			assertBrowserSession(t, item, bindings[1].Reference, "connected")
		default:
			t.Fatal("two-account agent unexpectedly created a third session")
		}
	}
	// A new execution on B must retain its own cookie and saved model selection.
	command := publishBrowse(t, peer.bus, bindings[1], site.URL+"/account", 4)
	assertAccountBrowse(t, framework, command, bindings[1], site.Accounts[1].Label, protected)
	if models[0].Calls() != 2 || models[1].Calls() != 4 || site.Submissions.Load() != 2 {
		t.Fatal("closing account A disrupted B, repeated a login or replayed model work")
	}
	inspected := peer.control(bindings[1], "inspect")
	encoded, err := json.Marshal(inspected)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountValuesAbsent(t, encoded, protected)
	for _, message := range peer.pending {
		assertAccountValuesAbsent(t, message.Payload, protected)
	}
}

func assertAccountBrowse(t *testing.T, framework *browseFramework, command messages.Message, binding sessions.Binding, label string, protected []string) {
	t.Helper()
	output := receiveBrowse(t, framework.peer, command, protected...)
	var findings actions.BrowseOutput
	if json.Unmarshal(output.Output, &findings) != nil || findings.Status != "completed" || output.Session == nil || len(findings.Findings) != 1 || findings.Findings[0].Text != label || len(findings.Evidence) != 1 || !strings.Contains(findings.Evidence[0].Text, label) {
		t.Fatal("account browse did not return its own grounded identity")
	}
	assertBrowserSession(t, *output.Session, binding.Reference, "connected")
	checkBrowseArtifacts(t, framework.files, output, protected...)
}

func assertAccountValuesAbsent(t *testing.T, data []byte, protected []string) {
	t.Helper()
	for _, value := range protected {
		if bytes.Contains(data, []byte(value)) {
			t.Error("an account value or distinctive prefix entered model/public data")
		}
	}
}
