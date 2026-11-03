//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func TestLoginThroughFrameworkThenBrowseRetainsAuthenticationAndRedactsCredentials(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	usernamePrefix, passwordPrefix := site.Username[:16], site.Password[:16]
	const key = "fixture-login-browse-model-key-c15ed9d3"
	model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		encoded, err := json.Marshal(turn)
		if err != nil || bytes.Contains(encoded, []byte(site.Username)) || bytes.Contains(encoded, []byte(site.Password)) || bytes.Contains(encoded, []byte(usernamePrefix)) || bytes.Contains(encoded, []byte(passwordPrefix)) {
			t.Error("managed login credentials entered model observations, targets or evidence")
		}
		if !strings.Contains(turn.Observation.Text, "Account verified") || !strings.Contains(turn.Observation.Text, "42 samples") {
			t.Error("browse did not retain the authenticated account")
		}
		return finishOrExtractStudy(turn)
	})
	framework := startBrowseFramework(t, key)
	peer := framework.peer
	binding := newBrowserSessionBinding(framework.host.ID())
	input := loginInput(binding, site.URL, "/login", "work")
	delete(input, "credential_profile")
	command := publishLogin(t, peer, binding, input)
	answerBrowseQuestion(t, peer, command, model, "browser_login_profile", "work", key, nil, false)
	usernameName, passwordName := loginSecretNames(site.URL, "work")
	deliverBrowseModelSecret(t, peer, model, usernameName, site.Username)
	deliverBrowseModelSecret(t, peer, model, passwordName, site.Password)
	output := receiveBrowse(t, peer, command, site.Username, site.Password, usernamePrefix, passwordPrefix, key)
	var login actions.LoginOutput
	if err := json.Unmarshal(output.Output, &login); err != nil {
		t.Fatal(err)
	}
	if login.Engine != "lightpanda" || login.Status != "verified" || !login.Submitted || site.Submissions.Load() != 1 || len(output.Artifacts) != 0 || output.Session == nil || model.Calls() != 0 {
		t.Fatal("managed login did not verify exactly one native submission before browsing")
	}
	assertBrowserSession(t, *output.Session, binding.Reference, "connected")
	binding.Mode = sessions.Reuse
	browseCommand := publishBrowse(t, peer.bus, binding, site.URL+"/account", 4)
	for _, setup := range []struct {
		name, answer string
		options      []string
	}{
		{name: "browser_model_provider", answer: "other"},
		{name: "browser_model_name", answer: "fixture-model"},
		{name: "browser_model_url", answer: model.URL},
	} {
		options := setup.options
		if setup.name == "browser_model_provider" {
			options = []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}
		}
		answerBrowseQuestion(t, peer, browseCommand, model, setup.name, setup.answer, key, options, false)
	}
	deliverBrowseModelSecret(t, peer, model, modelSetupSecretName(model.URL), key)
	browsed := receiveBrowse(t, peer, browseCommand, site.Username, site.Password, usernamePrefix, passwordPrefix, key)
	var findings actions.BrowseOutput
	if err := json.Unmarshal(browsed.Output, &findings); err != nil {
		t.Fatal(err)
	}
	if findings.Status != "completed" || len(findings.Evidence) != 1 || !strings.Contains(findings.Evidence[0].Text, "42 samples") || model.Calls() != 2 || site.Submissions.Load() != 1 || browsed.Session == nil || browsed.Session.ID != binding.ID {
		t.Fatal("login reuse lost authentication, repeated submission or omitted grounded findings")
	}
	checkBrowseArtifacts(t, framework.files, browsed, site.Username, site.Password, usernamePrefix, passwordPrefix, key)
	inspected := peer.control(binding, "inspect")
	encoded, err := json.Marshal(inspected)
	if err != nil || bytes.Contains(encoded, []byte(site.Username)) || bytes.Contains(encoded, []byte(site.Password)) || bytes.Contains(encoded, []byte(usernamePrefix)) || bytes.Contains(encoded, []byte(passwordPrefix)) {
		t.Fatal("login credentials entered session inventory")
	}
	for _, message := range peer.pending {
		if bytes.Contains(message.Payload, []byte(site.Username)) || bytes.Contains(message.Payload, []byte(site.Password)) || bytes.Contains(message.Payload, []byte(usernamePrefix)) || bytes.Contains(message.Payload, []byte(passwordPrefix)) {
			t.Fatal("login credentials entered public questions or progress events")
		}
	}
	// A separate Commander identity starts signed out even on the same origin.
	independent := newBrowserSessionBinding(framework.host.ID())
	const isolatedKey = "fixture-isolated-login-model-key-6912c04d"
	isolatedModel := fixtures.NewBrowseModel(t, isolatedKey, func(turn intelligence.Turn) intelligence.Command {
		if !strings.Contains(turn.Observation.Text, "Signed out") || strings.Contains(turn.Observation.Text, "Account verified") {
			t.Error("independent browser session inherited login authentication")
		}
		if len(turn.Evidence) == 0 {
			return intelligence.Command{Action: "extract"}
		}
		return intelligence.Command{Action: "finish", Summary: "The isolated account is signed out.", Findings: []intelligence.Finding{{Text: "The account is signed out.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
	})
	isolatedCommand := publishBrowse(t, peer.bus, independent, site.URL+"/account", 4)
	answerBrowseQuestion(t, peer, isolatedCommand, isolatedModel, "browser_model_provider", "other", isolatedKey, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, false)
	answerBrowseQuestion(t, peer, isolatedCommand, isolatedModel, "browser_model_name", "fixture-model", isolatedKey, nil, false)
	answerBrowseQuestion(t, peer, isolatedCommand, isolatedModel, "browser_model_url", isolatedModel.URL, isolatedKey, nil, false)
	deliverBrowseModelSecret(t, peer, isolatedModel, modelSetupSecretName(isolatedModel.URL), isolatedKey)
	isolated := receiveBrowse(t, peer, isolatedCommand, site.Username, site.Password, isolatedKey)
	if isolated.Session == nil || isolated.Session.ID == binding.ID {
		t.Fatal("independent browsing reused the authenticated browser identity")
	}
}

func TestLoginRejectsUnsafeFormsAndChallengesBeforeRequestingCredentials(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := realLogin(t, manager)
	for _, test := range []struct{ path, status string }{{"/foreign", "unsupported_form"}, {"/override", "unsupported_form"}, {"/challenge", "requires_challenge"}} {
		t.Run(strings.TrimPrefix(test.path, "/"), func(t *testing.T) {
			secrets, logs := &browseSecretProbe{}, &loginLogProbe{}
			ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), logs)
			result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, test.path, "work")))
			var login actions.LoginOutput
			if err != nil || json.Unmarshal(result.Output, &login) != nil || login.Status != test.status || login.Submitted || secrets.requests != 0 || site.Submissions.Load() != 0 || site.ForeignHits.Load() != 0 || result.Session == nil {
				t.Fatal("unsafe or challenged login requested credentials, submitted a form or lost its browser")
			}
			assertBrowserSession(t, *result.Session, binding.Reference, "connected")
		})
		binding.Mode = sessions.Reuse
	}
}

func TestLoginManagedCredentialNamesAreBoundToOriginAndProfile(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := realLogin(t, manager)
	seen := map[string]bool{}
	for _, test := range []struct{ origin, profile string }{{site.URL, "work"}, {site.URL, "personal"}, {strings.Replace(site.URL, "127.0.0.1", "localhost", 1), "work"}} {
		secrets := &browseSecretProbe{}
		ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), &loginLogProbe{})
		result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, test.origin, "/login", test.profile)))
		username, _ := loginSecretNames(test.origin, test.profile)
		if err == nil || !slices.Equal(secrets.names, []string{username}) || seen[username] || site.Submissions.Load() != 0 || result.Session == nil {
			t.Fatal("login reused an unbound origin/profile credential or submitted without credentials")
		}
		seen[username] = true
		assertBrowserSession(t, *result.Session, binding.Reference, "connected")
		binding.Mode = sessions.Reuse
	}
}

func TestLoginStopsAtChallengeAfterPasswordSubmission(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := realLogin(t, manager)
	username, password := loginSecretNames(site.URL, "work")
	secrets := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(site.Password)}}
	ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), &loginLogProbe{})
	result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, "/challenge-after", "work")))
	var login actions.LoginOutput
	if err != nil || json.Unmarshal(result.Output, &login) != nil || login.Status != "requires_challenge" || !login.Submitted || !slices.Equal(secrets.names, []string{username, password}) || site.Submissions.Load() != 1 || result.Session == nil {
		t.Fatal("password submission did not stop at its challenge without requesting a code")
	}
	assertBrowserSession(t, *result.Session, binding.Reference, "connected")
	binding.Mode = sessions.Reuse
	var retainedChallenge bool
	_, err = manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(`location.pathname === "/otp" && document.querySelector('input[autocomplete="one-time-code"]') !== null`, &retainedChallenge))
	})
	if err != nil || !retainedChallenge || site.Submissions.Load() != 1 {
		t.Fatal("login navigated away from the retained challenge page or retried submission")
	}
}

func TestLoginVerificationMarkerCannotOverrideVisibleChallenge(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := realLogin(t, manager)
	username, password := loginSecretNames(site.URL, "work")
	for _, test := range []struct {
		path      string
		submitted bool
	}{{path: "/account-challenge"}, {path: "/account-stepup", submitted: true}} {
		t.Run(strings.TrimPrefix(test.path, "/"), func(t *testing.T) {
			secrets := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(site.Password)}}
			input := loginInput(binding, site.URL, "/login", "work")
			input["verification_url"] = site.URL + test.path
			ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), &loginLogProbe{})
			result, err := executor.Run(ctx, marshalLoginInput(t, input))
			var login actions.LoginOutput
			if err != nil || json.Unmarshal(result.Output, &login) != nil || login.Status != "requires_challenge" || login.Submitted != test.submitted || result.Session == nil {
				t.Fatal("verification marker overrode a visible authentication challenge")
			}
			wantNames, wantPosts := []string(nil), int32(0)
			if test.submitted {
				wantNames, wantPosts = []string{username, password}, 1
			}
			if !slices.Equal(secrets.names, wantNames) || site.Submissions.Load() != wantPosts {
				t.Fatal("verification challenge requested a code, submitted unnecessarily or repeated login")
			}
			assertBrowserSession(t, *result.Session, binding.Reference, "connected")
		})
		binding.Mode = sessions.Reuse
	}
}

func TestLoginCancellationDuringManagedSecretWaitPreservesSession(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := realLogin(t, manager)
	secrets := &browseSecretProbe{wait: true, entered: make(chan string, 1)}
	work, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := action.WithLogs(action.WithSecrets(action.WithSession(work, &binding), secrets), &loginLogProbe{})
	type outcome struct {
		result action.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, "/login", "work")))
		done <- outcome{result, err}
	}()
	select {
	case <-secrets.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("login did not wait for its managed username")
	}
	cancel()
	select {
	case stopped := <-done:
		if !errors.Is(stopped.err, context.Canceled) || stopped.result.Session == nil || site.Submissions.Load() != 0 {
			t.Fatal("cancelled credential request submitted a form or lost cancellation")
		}
		assertBrowserSession(t, *stopped.result.Session, binding.Reference, "connected")
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled login did not return promptly")
	}
	binding.Mode = sessions.Reuse
	username, password := loginSecretNames(site.URL, "work")
	resolved := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(site.Password)}}
	ctx = action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), resolved), &loginLogProbe{})
	result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, "/login", "work")))
	var login actions.LoginOutput
	if err != nil || json.Unmarshal(result.Output, &login) != nil || login.Status != "verified" || site.Submissions.Load() != 1 {
		t.Fatal("cancelled managed login left its retained browser unusable")
	}
	for _, returned := range resolved.returned {
		for _, value := range returned {
			if value != 0 {
				t.Fatal("login retained a framework-owned credential buffer")
			}
		}
	}
}

func TestLoginStopsAfterInvalidCredentialsOrUncertainSubmission(t *testing.T) {
	_ = browserSessionOptions(t)
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_password", true: "dropped_post_response"}[uncertain], func(t *testing.T) {
			site := fixtures.NewLoginSite(t)
			manager := newBrowserSessionManager(t)
			binding := newBrowserSessionBinding(manager.owner)
			if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
				t.Fatal(err)
			}
			counter := &loginRequestCounter{Manager: manager.Manager}
			var runner actions.BrowserRunner = manager
			if uncertain {
				runner = counter
			}
			executor := realLogin(t, runner)
			username, password := loginSecretNames(site.URL, "work")
			passwordValue, path := "", "/login"
			if uncertain {
				passwordValue, path = site.Password, "/drop"
			}
			secrets := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(passwordValue)}}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			ctx = action.WithLogs(action.WithSecrets(action.WithSession(ctx, &binding), secrets), &loginLogProbe{})
			result, err := executor.Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, path, "work")))
			if !uncertain {
				if err == nil || site.Submissions.Load() != 0 {
					t.Fatal("invalid managed password reached native form submission")
				}
			} else {
				var login actions.LoginOutput
				// Lightpanda 1.0.2 may replay below CDP after a disconnected
				// response. Prove the agent issued one browser request, while
				// retaining the actual HTTP count without a wire-once promise.
				if err != nil || json.Unmarshal(result.Output, &login) != nil || login.Status != "submission_uncertain" || !login.Submitted || site.Submissions.Load() < 1 || counter.fetchPosts.Load() != 1 || counter.networkPosts.Load() != 1 || counter.runs.Load() != 1 || !slices.Equal(secrets.names, []string{username, password}) {
					t.Fatalf("uncertain submission did not stop after one owned browser request: error=%v status=%q submitted=%t posts=%d fetch_posts=%d network_posts=%d browser_runs=%d credential_requests=%d", err, login.Status, login.Submitted, site.Submissions.Load(), counter.fetchPosts.Load(), counter.networkPosts.Load(), counter.runs.Load(), secrets.requests)
				}
			}
			for _, returned := range secrets.returned {
				for _, value := range returned {
					if value != 0 {
						t.Fatal("login retained a returned credential buffer after failure")
					}
				}
			}
		})
	}
}

func TestLoginCloseInterruptsPendingManagedCredential(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	const modelKey = "fixture-unused-login-close-model-5c81e6"
	framework := startBrowseFramework(t, modelKey)
	peer := framework.peer
	binding := newBrowserSessionBinding(framework.host.ID())
	command := publishLogin(t, peer, binding, loginInput(binding, site.URL, "/login", "work"))
	request := peer.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.SecretSyncRequested || message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.CommandFailed || message.Type == controllerevents.ActionSucceeded
	})
	if request.Type != controllerevents.SecretSyncRequested || site.Submissions.Load() != 0 {
		t.Fatal("login did not wait for a managed credential before submission")
	}
	closed := peer.control(binding, "close")
	if len(closed.Sessions) != 1 {
		t.Fatal("closing pending login discarded its reference")
	}
	assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
	outcome := peer.wait(func(message messages.Message) bool {
		return message.CorrelationID == command.ID && (message.Type == controllerevents.CommandFailed || message.Type == controllerevents.ActionSucceeded)
	})
	if outcome.Type != controllerevents.CommandFailed || site.Submissions.Load() != 0 {
		t.Fatal("closed pending login resumed a credential-bearing submission")
	}
	peer.accept(outcome)
}

func realLogin(t *testing.T, runner actions.BrowserRunner) *actions.Login {
	t.Helper()
	executor := actions.NewLogin(runner)
	status, err := executor.Discover(t.Context(), &environment.Local{}, nil)
	if err != nil || status != action.Ready {
		t.Fatal("real login discovery failed")
	}
	return executor
}

type loginRequestCounter struct {
	*browserruntime.Manager
	runs         atomic.Int32
	fetchPosts   atomic.Int32
	networkPosts atomic.Int32
}

func (counter *loginRequestCounter) Run(ctx context.Context, options browserruntime.Options, work func(context.Context) error) (sessions.Session, error) {
	counter.runs.Add(1)
	return counter.Manager.Run(ctx, options, func(ctx context.Context) error {
		chromedp.ListenTarget(ctx, func(event any) {
			switch event := event.(type) {
			case *fetch.EventRequestPaused:
				if event.Request != nil && event.Request.Method == "POST" {
					counter.fetchPosts.Add(1)
				}
			case *network.EventRequestWillBeSent:
				if event.Request != nil && event.Request.Method == "POST" {
					counter.networkPosts.Add(1)
				}
			}
		})
		return work(ctx)
	})
}

func publishLogin(t *testing.T, peer *browserSessionPeer, binding sessions.Binding, input map[string]any) messages.Message {
	t.Helper()
	work := messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: id.TaskID()}
	if binding.Mode == sessions.Create {
		work.TaskID = binding.Creation.TaskID
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "login", Input: marshalLoginInput(t, input), Work: work, Session: &binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: peer.bus}).Publish(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	return command
}
