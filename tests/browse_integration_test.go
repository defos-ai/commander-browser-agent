//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/comms"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
)

func TestBrowseBlocksForeignRedirectAndReleasesNavigationGuard(t *testing.T) {
	options := browserSessionOptions(t)
	site, foreignURL, hits, redirects := fixtures.NewBrowseScopeSite(t)
	const key = "fixture-scope-model-key-02da2809"
	modelServer := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if strings.Contains(turn.Observation.Text, "foreign-document-canary") {
			t.Error("foreign document reached the model")
		}
		for _, item := range turn.Observation.Targets {
			if strings.Contains(item.Label, "Follow redirect") {
				return intelligence.Command{Action: "click", Target: item.ID}
			}
		}
		return intelligence.Command{Action: "finish", Summary: "Stopped after redirect."}
	})
	model, err := intelligence.NewModel(intelligence.ModelConfig{URL: modelServer.URL, Name: "fixture-model"}, []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := model.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		_, err := intelligence.Run(ctx, intelligence.Request{Objective: "Follow the redirect within the permitted website.", URL: site.URL, MaxSteps: 2, MaxCompletionTokens: 2048}, model, intelligence.Hooks{})
		return err
	})
	if modelServer.Calls() == 0 || redirects.Load() == 0 {
		t.Fatal("scope probe did not reach the redirect through a model-selected click")
	}
	if hits.Load() != 0 {
		t.Fatal("out-of-scope document was requested before being blocked")
	}
	// Finishing execution must remove interception rather than leave the
	// reusable browser frozen. A later owner-authorized CDP call can navigate.
	binding.Mode = sessions.Reuse
	_, err = manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Navigate(foreignURL+"/authorized-after-browse"))
	})
	if err != nil {
		t.Fatalf("browse left navigation interception installed: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("subsequent authorized navigation did not reach its target")
	}
}

func TestBrowseThroughFrameworkManagedModelAndLightpanda(t *testing.T) {
	_ = browserSessionOptions(t) // Fail explicitly when the real runtime is absent.
	for _, test := range []struct {
		name      string
		legacyEnv bool
	}{
		{name: "questions_then_endpoint_managed_key"},
		{name: "legacy_model_environment_ignored", legacyEnv: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearLegacyModelEnvironment(t)
			const key = "fixture-managed-model-key-8c6737a9"
			const ignoredKey = "fixture-ignored-env-key-163dab"
			if test.legacyEnv {
				t.Setenv("BROWSER_MODEL_PROVIDER", "unsupported-legacy-provider")
				t.Setenv("BROWSER_MODEL_NAME", "ignored-env-model")
				t.Setenv("BROWSER_MODEL_URL", "https://env.example.invalid/v1/chat/completions")
				for _, name := range []string{"BROWSER_MODEL_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY", "DEEPSEEK_API_KEY"} {
					t.Setenv(name, ignoredKey)
				}
			}
			site := fixtures.NewBrowseSite(t, key)
			model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
				observed, err := json.Marshal(turn)
				if err != nil || bytes.Contains(observed, []byte(ignoredKey)) || bytes.Contains(observed, []byte("ignored-env-model")) {
					t.Error("legacy model environment entered the model turn")
				}
				if len(turn.Evidence) > 0 {
					return intelligence.Command{Action: "finish", Summary: "Found the requested product price.", Findings: []intelligence.Finding{{
						Text: "The solar lamp costs 24 credits.", EvidenceIDs: []string{turn.Evidence[0].ID},
					}}}
				}
				if strings.Contains(turn.Observation.Text, "The solar lamp costs 24 credits.") {
					return intelligence.Command{Action: "extract"}
				}
				if len(turn.RecentSteps) > 0 && turn.RecentSteps[len(turn.RecentSteps)-1].Action == "type_text" {
					for _, target := range turn.Observation.Targets {
						if strings.Contains(target.Label, "Search catalog") {
							return intelligence.Command{Action: "click", Target: target.ID}
						}
					}
				}
				if len(turn.RecentSteps) > 0 && (turn.RecentSteps[len(turn.RecentSteps)-1].Action == "click" || turn.RecentSteps[len(turn.RecentSteps)-1].Action == "wait") {
					return intelligence.Command{Action: "wait"}
				}
				for _, target := range turn.Observation.Targets {
					if strings.Contains(target.Label, "Search fixtures") {
						return intelligence.Command{Action: "type_text", Target: target.ID, Text: "solar lamp"}
					}
				}
				t.Error("fixture model could not identify a non-secret search field")
				return intelligence.Command{Action: "finish", Summary: "No search field."}
			})
			framework := startBrowseFramework(t, key)
			peer := framework.peer
			binding := newBrowserSessionBinding(framework.host.ID())
			command := publishBrowse(t, peer.bus, binding, site.URL, 12)
			answerBrowseQuestion(t, peer, command, model, "browser_model_provider", "More providers", key, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, true)
			answerBrowseQuestion(t, peer, command, model, "browser_model_provider_more", "Other (OpenAI-compatible)", key, []string{"xAI", "DeepSeek", "Other (OpenAI-compatible)"}, false)
			answerBrowseQuestion(t, peer, command, model, "browser_model_name", "fixture-model", key, nil, false)
			answerBrowseQuestion(t, peer, command, model, "browser_model_url", model.URL, key, nil, false)
			deliverBrowseModelSecret(t, peer, model, actions.ModelSecretName(model.URL), key)
			output := receiveBrowse(t, peer, command, key, ignoredKey)
			var findings actions.BrowseOutput
			if err := json.Unmarshal(output.Output, &findings); err != nil {
				t.Fatal(err)
			}
			if findings.Engine != "lightpanda" || findings.Status != "completed" || len(findings.Findings) != 1 || len(findings.Evidence) != 1 || !strings.Contains(findings.Evidence[0].Text, "24 credits") {
				t.Fatalf("browse did not return grounded structured findings: %+v", findings)
			}
			if findings.Usage.Steps < 4 || findings.Usage.ModelCalls != model.Calls() || findings.Usage.CompletionTokensReserved > 8192 {
				t.Fatalf("browse did not account for its model execution: %+v", findings.Usage)
			}
			if output.Session == nil {
				t.Fatal("browse omitted its reusable session")
			}
			assertBrowserSession(t, *output.Session, binding.Reference, "connected")
			checkBrowseArtifacts(t, framework.files, output, key, ignoredKey)

			// Reuse the same browser with a one-step budget. The resulting partial report
			// is still stored, and the original session identity stays connected.
			binding.Mode = sessions.Reuse
			boundedCommand := publishBrowse(t, peer.bus, binding, site.URL, 1)
			boundedOutput := receiveBrowse(t, peer, boundedCommand, key, ignoredKey)
			if err := json.Unmarshal(boundedOutput.Output, &findings); err != nil {
				t.Fatal(err)
			}
			if findings.Status != "budget_exhausted" || findings.Usage.Steps != 1 || boundedOutput.Session == nil || boundedOutput.Session.ID != binding.ID {
				t.Fatalf("browse did not stop at its step budget with a retained session: %+v", findings)
			}
			checkBrowseArtifacts(t, framework.files, boundedOutput, key, ignoredKey)
			closed := peer.control(binding, "close")
			if len(closed.Sessions) != 1 {
				t.Fatal("close discarded the browse session reference")
			}
			assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
			logs := 0
			for _, message := range peer.pending {
				if bytes.Contains(message.Payload, []byte(key)) || bytes.Contains(message.Payload, []byte(ignoredKey)) {
					t.Fatal("model credential entered a public event")
				}
				if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
					t.Fatal("reused browsing requested unexpected setup or credentials")
				}
				if message.Type == controllerevents.ExecutionLogged {
					if _, err := controllerevents.DecodeExecutionLog(message); err != nil {
						t.Fatal(err)
					}
					logs++
				}
			}
			if logs == 0 {
				t.Fatal("browse emitted no Commander execution progress")
			}
		})
	}
}

func publishBrowse(t *testing.T, bus comms.Bus, binding sessions.Binding, targetURL string, maxSteps int) messages.Message {
	t.Helper()
	input := map[string]any{"objective": "Find the solar lamp price using the catalog search.", "url": targetURL, "engine": "lightpanda", "session_scope": binding.ScopeID, "max_steps": maxSteps}
	if binding.Mode == sessions.Reuse {
		input["session_id"] = binding.ID
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	work := messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: id.TaskID()}
	if binding.Mode == sessions.Create {
		work.TaskID = binding.Creation.TaskID
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "browse", Input: raw, Work: work, Session: &binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: bus}).Publish(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	return command
}

func receiveBrowse(t *testing.T, peer *browserSessionPeer, command messages.Message, credentials ...string) agent.ActionOutput {
	t.Helper()
	message := peer.wait(func(message messages.Message) bool {
		return message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
	})
	for _, credential := range credentials {
		if bytes.Contains(message.Payload, []byte(credential)) {
			t.Fatal("model credential entered a public action outcome")
		}
	}
	if message.Type != controllerevents.ActionSucceeded {
		t.Fatal("browse failed")
	}
	var output agent.ActionOutput
	if err := json.Unmarshal(message.Payload, &output); err != nil {
		t.Fatal(err)
	}
	if err := output.Validate(); err != nil {
		t.Fatal(err)
	}
	peer.accept(message)
	return output
}

func checkBrowseArtifacts(t *testing.T, files *fsartifacts.ArtifactFileFsStore, output agent.ActionOutput, credentials ...string) {
	t.Helper()
	if len(output.Artifacts) != 1 || len(output.Artifacts[0].Files) != 2 {
		t.Fatal("browse did not group its findings and evidence artifacts")
	}
	for _, credential := range credentials {
		if bytes.Contains(output.Output, []byte(credential)) {
			t.Fatal("model credential entered structured output")
		}
	}
	for _, file := range output.Artifacts[0].Files {
		data, err := files.Get(t.Context(), output.Artifacts[0].ID, file)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(data) || file.MediaType != "application/json" {
			t.Fatal("browse artifact is invalid")
		}
		for _, credential := range credentials {
			if bytes.Contains(data, []byte(credential)) {
				t.Fatal("browse artifact contains a model credential")
			}
		}
	}
}
