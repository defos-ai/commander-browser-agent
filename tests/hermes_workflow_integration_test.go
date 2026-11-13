//go:build hermesintegration

package browser_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	runtimeSessions "github.com/defos-ai/commander/framework/sessions"
)

// This uses the real default catalog, not test-owned action adapters. Its root
// browse cannot start its Hermes child until the start checkpoint is acknowledged.
func TestHermesIntegratedManagedWorkflow(t *testing.T) {
	site := fixtures.NewBrowser(t)
	label := site.Accounts[0].Label
	key := "fixture-managed-workflow-key-" + id.New()
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Code, key}
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-study", "browser_exec", map[string]any{
				"code": fmt.Sprintf("new_tab(%q)\nprint(js('document.body.textContent'))", site.URL+"/details"),
			})}}
		case 2:
			if !request.HasToolText("42 samples") {
				t.Error("native reasoning lost its study observation")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-question", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which workflow figure should be reported?", "choices": []string{"Report the sample count", "Report the account label"}}},
			})}}
		case 3:
			if !request.HasToolText("Report the sample count") {
				t.Error("Commander did not deliver the workflow clarification answer")
			}
			return fakes.HermesResponse{Text: "The study measured 42 samples. Provider echo " + key, Escape: key}
		case 4, 6:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-account", "browser_exec", map[string]any{
				"code": fmt.Sprintf("goto_url(%q)\nprint(js('document.body.textContent'))\ngoto_url(%q)\nprint(js('document.body.textContent'))\ngoto_url(%q)", site.URL+"/otp-account", site.URL+"/details", site.URL+"/otp-account"),
			})}}
		case 5, 7:
			if !request.HasToolText(label) || !request.HasToolText("42 samples") || !request.HasToolText("[REDACTED]") {
				t.Error("native reuse lost its authenticated account or protected observations")
			}
			return fakes.HermesResponse{Text: "Authenticated workflow reports 42 samples. Provider echo " + key, Escape: key}
		default:
			t.Error("the managed workflow exceeded its scripted model requests")
			return fakes.HermesResponse{Text: "Unexpected workflow continuation."}
		}
	}, protected[:3]...)
	endpoint := model.URL + "/chat/completions"
	secretName := modelSecretName(endpoint)
	h := newDefaultHermesSessionHost(t)
	profilesBefore, processesBefore := hermesSessionProfiles(t), hermesReadinessProcesses(t)
	input := browsing.HermesBrowseInput{Objective: "Read the workflow study and report its sample count.", URL: site.URL + "/details",
		MaxIterations: 4, MaxTokensPerResponse: 512, TimeoutSeconds: 35}
	first := h.nativeBrowseTask(nil, input)
	binding := h.binding(first)
	firstBinding := binding
	command := h.dispatch(first, &firstBinding)
	h.answerSetup(command, "browser_model_provider", "Other (OpenAI-compatible)")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	h.answerSetup(command, "browser_model_url", endpoint)
	if model.Calls() != 0 {
		t.Fatal("the managed workflow contacted a model before managed key delivery")
	}
	lease := h.deliverModelKey(command, secretName, key, 1)
	started := h.checkpoint(controllerevents.CheckpointSave, "browse_started")
	if model.Calls() != 0 || h.snapshot(binding.Reference).State != "starting" ||
		!reflect.DeepEqual(profilesBefore, hermesSessionProfiles(t)) || !reflect.DeepEqual(processesBefore, hermesReadinessProcesses(t)) {
		t.Fatal("managed browse crossed its unconfirmed startup/native-work fence")
	}
	h.replyCheckpoint(started, h.persistCheckpoint(started))
	questionFence := h.checkpoint(controllerevents.CheckpointSave, "browse_step_02")
	if model.Calls() != 2 {
		t.Fatal("native clarification did not stop at its persistence fence")
	}
	for _, message := range h.history {
		if message.Type == controllerevents.QuestionRequested {
			question, err := controllerevents.DecodeQuestionRequest(message)
			hermesSessionCheck(t, err)
			if question.CommandID == command.ID && question.Key == "browser_clarification_1" {
				t.Fatal("native clarification was published before its checkpoint receipt")
			}
		}
	}
	h.replyCheckpoint(questionFence, h.persistCheckpoint(questionFence))
	h.answerSetup(command, "browser_clarification_1", "Report the sample count")
	final := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	firstOutput, ok := h.screenshotResult(command)
	requireHermesWorkflowBrowse(t, h, firstOutput, ok, binding, 3, protected...)
	assertHermesCheckpointFacts(t, h, command, firstBinding, append(protected, site.URL, input.Objective, "Which workflow figure should be reported?", "Report the sample count", "42 samples")...)
	if len(h.checkpoints[command.ID].Checkpoints) != 4 || model.Calls() != 3 {
		t.Fatal("integrated browse omitted its shared question/batch fences or repeated reasoning")
	}

	loginTask := defaultHermesLoginTask(t, h, &first, actions.LoginInput{URL: site.URL + "/otp-login", VerificationURL: site.URL + "/otp-account",
		ExpectedText: label, CredentialProfile: site.Accounts[0].Profile, TimeoutSeconds: 35})
	loginBinding := h.binding(loginTask)
	command = h.dispatch(loginTask, &loginBinding)
	started = h.checkpoint(controllerevents.CheckpointSave, "login_started")
	assertHermesWorkflowPosts(t, site, 0, 0)
	if model.Calls() != 3 {
		t.Fatal("deterministic login performed model setup or reasoning")
	}
	h.replyCheckpoint(started, h.persistCheckpoint(started))
	h.loginCredentials(command, site, 0)
	codeRequest := h.loginCodeRequest(command, site)
	assertHermesWorkflowPosts(t, site, 1, 0)
	if model.Calls() != 3 {
		t.Fatal("login credential delivery reached native model reasoning")
	}
	h.loginCode(codeRequest, site.Code, "")
	final = h.checkpoint(controllerevents.CheckpointSave, "login_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	loggedIn, ok := h.screenshotResult(command)
	if !ok {
		t.Fatal("managed login did not complete")
	}
	var loginOutput actions.LoginOutput
	if json.Unmarshal(loggedIn.Output, &loginOutput) != nil {
		t.Fatal("the production login omitted its native result")
	}
	requireNativeLogin(t, hermesSessionOutput{Login: &loginOutput.Result}, "verified", true)
	if loggedIn.Session == nil || loggedIn.Session.Reference != binding.Reference || loginBinding.Claim() != binding.Claim() || len(h.checkpoints[command.ID].Checkpoints) != 2 {
		t.Fatal("managed login changed its original claim or omitted recovery facts")
	}
	assertHermesCheckpointFacts(t, h, command, loginBinding, append(protected, site.URL, label)...)
	assertHermesWorkflowPosts(t, site, 1, 1)

	waitHermesBrowseLease(t, lease)
	input.Objective, input.URL = "Inspect the retained signed-in workflow account and report the study's sample count.", site.URL+"/otp-account"
	authenticated := h.nativeBrowseTask(&loginTask, input)
	authenticatedBinding := h.binding(authenticated)
	command = h.dispatch(authenticated, &authenticatedBinding)
	lease = h.deliverModelKey(command, secretName, key, 2)
	final = h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	authenticatedOutput, ok := h.screenshotResult(command)
	requireHermesWorkflowBrowse(t, h, authenticatedOutput, ok, binding, 2, protected...)
	if model.Calls() != 5 || authenticatedBinding.Claim() != binding.Claim() {
		t.Fatal("managed native reuse lost its authenticated browser or repeated setup")
	}
	assertHermesCheckpointFacts(t, h, command, authenticatedBinding, append(protected, site.URL, input.Objective, label)...)
	beforeCapture, events := model.Calls(), len(h.history)
	firstPixels, _ := captureHermesScreenshot(t, h, authenticated, "desktop")
	use := authenticatedBinding
	use.Mode = sessions.Use
	// Replace only known echoes in the retained document. Fetching and parsing
	// another page could change authentication or DOM structure independently
	// of the renderer's protection, making its pixels an ambiguous reference.
	hermesScreenshotEvaluate(t, h, use, `(()=>{const echo=document.querySelector('p');if(!echo||document.querySelector('h1')?.textContent!=='Account Alpha verified')throw new Error('invalid_workflow_reference');echo.textContent='Credential echo [REDACTED] [REDACTED] [REDACTED]';return true})()`)
	if redactedPixels, _ := captureHermesScreenshot(t, h, authenticated, "desktop"); firstPixels != redactedPixels {
		t.Fatal("integrated screenshot pixels disclosed managed credentials or the transient code")
	}
	if model.Calls() != beforeCapture {
		t.Fatal("managed screenshot capture invoked a model")
	}
	for _, message := range h.history[events:] {
		if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
			if _, answered := h.accepted[message.ID]; !answered {
				t.Fatal("managed screenshot asked for setup or managed credentials")
			}
		}
	}

	// Native work may finish while final persistence is refused. Prepared text
	// and artifacts remain suppressed, and this exact live session is retained.
	waitHermesBrowseLease(t, lease)
	refused := h.nativeBrowseTask(&authenticated, input)
	refusedBinding := h.binding(refused)
	command = h.dispatch(refused, &refusedBinding)
	h.deliverModelKey(command, secretName, key, 3)
	final = h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.refuseCheckpoint(final)
	assertHermesCheckpointFailure(t, h, command)
	if model.Calls() != 7 {
		t.Fatal("managed browse repeated model work after its final persistence refusal")
	}
	if h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("failed native final persistence lost browser ownership")
	}
	assertHermesCheckpointFacts(t, h, command, refusedBinding, append(protected, site.URL, input.Objective, label)...)
	assertHermesWorkflowPosts(t, site, 1, 1)
	assertNativeLoginPrivate(t, h, protected...)

	// An independent root with an old saved start fence must fail before model
	// setup and before a worker is started under its fresh creation claim.
	replay := h.nativeBrowseTask(nil, input)
	replayBinding := h.binding(replay)
	profilesBeforeReplay, processesBeforeReplay := hermesSessionProfiles(t), hermesReadinessProcesses(t)
	beforeReplay := len(h.history)
	command = h.dispatch(replay, &replayBinding)
	load := h.checkpoint(controllerevents.CheckpointLoad, "browse_started")
	record := h.checkpoints[command.ID]
	_, err := record.RecordCheckpoint("browse_started", json.RawMessage(`{"version":2,"phase":"browser_work_pending"}`))
	hermesSessionCheck(t, err)
	h.checkpoints[command.ID] = record
	h.replyCheckpoint(load, h.persistCheckpoint(load))
	assertHermesCheckpointFailure(t, h, command)
	if model.Calls() != 7 || !reflect.DeepEqual(profilesBeforeReplay, hermesSessionProfiles(t)) || !reflect.DeepEqual(processesBeforeReplay, hermesReadinessProcesses(t)) {
		t.Fatal("saved-fence rejection started model work or a native browser")
	}
	for _, message := range h.history[beforeReplay:] {
		if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
			if _, answered := h.accepted[message.ID]; !answered {
				t.Fatal("saved-fence rejection entered model setup or secret delivery")
			}
		}
	}
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" || !reflect.DeepEqual(profilesBefore, hermesSessionProfiles(t)) {
		t.Fatal("the integrated managed workflow did not confirm session and process closure")
	}
	closed := h.nativeBrowseTask(&authenticated, input)
	closedBinding := h.binding(closed)
	command = h.dispatch(closed, &closedBinding)
	assertHermesCheckpointFailure(t, h, command)
	if model.Calls() != 7 || !reflect.DeepEqual(profilesBefore, hermesSessionProfiles(t)) {
		t.Fatal("a closed production session reached the model or recreated its worker")
	}
}

func requireHermesWorkflowBrowse(t *testing.T, h *hermesSessionHost, output agent.ActionOutput, ok bool, binding sessions.Binding, requests int, protected ...string) {
	t.Helper()
	var result browsing.HermesBrowseOutput
	if !ok || json.Unmarshal(output.Output, &result) != nil || result.Validate() != nil || result.Status != "completed" || result.ModelRequests != requests ||
		!strings.Contains(result.Text, "42 samples") || !strings.Contains(result.Text, "[REDACTED]") || output.Session == nil || output.Session.Reference != binding.Reference ||
		len(output.Artifacts) != 1 || len(output.Artifacts[0].Files) != 1 || !bytes.Equal(output.Output, h.screenshotDownload(output, "browse.json")) {
		t.Fatal("integrated native browse lost its protected output, session or uploaded artifact")
	}
	for _, value := range protected {
		if bytes.Contains(output.Output, []byte(value)) {
			t.Fatal("a managed value entered a native browse artifact")
		}
	}
}

func assertHermesWorkflowPosts(t *testing.T, site *fixtures.BrowserSite, password, code int32) {
	t.Helper()
	if site.Posts.Load() != password || site.CodePosts.Load() != code {
		t.Fatal("the managed workflow crossed an unconfirmed login fence or repeated credential/code submission")
	}
}

func defaultHermesLoginTask(t *testing.T, h *hermesSessionHost, parent *operations.Task, input actions.LoginInput) operations.Task {
	t.Helper()
	raw, err := json.Marshal(input)
	hermesSessionCheck(t, err)
	next, err := runtimeSessions.Normalize(parent, []actionmodel.NextAction{{ActionName: actions.LoginName, Input: raw}}, nil, h.definitions...)
	hermesSessionCheck(t, err)
	return operations.NewTask(parent.OperationID, parent.ID, next[0])
}
