//go:build hermesintegration && hermescontainerintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

// The real Commander host composes the staged adapters. The production catalog
// remains unchanged until deployment configuration and the separate cutover.
func TestHermesContainerIntegratedManagedWorkflow(t *testing.T) {
	options := browserruntime.HermesOptions{Container: browserruntime.HermesContainerOptions{
		Socket: os.Getenv("HERMES_CONTAINER_TEST_SOCKET"), Image: os.Getenv("HERMES_CONTAINER_TEST_IMAGE"),
	}}
	if options.Container.Socket == "" || options.Container.Image == "" {
		t.Fatal("isolated workflow probes require an explicit daemon socket and pinned image ID")
	}
	api := newHermesDockerProbe(options.Container.Socket)
	t.Cleanup(api.client.CloseIdleConnections)
	var h *hermesSessionHost
	t.Cleanup(func() {
		if h != nil {
			for _, claim := range h.claims {
				cleanupHermesContainerSession(t, api, hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: claim})
			}
		}
	})
	const label = "Signed-in workflow account"
	site := &fixtures.BrowserSite{Code: "482719", Accounts: [2]fixtures.Account{{
		Profile: "workflow", Username: "workflow-private-user-427ac1", Password: "workflow-private-password-427ac1", Label: label,
	}}}
	key := "fixture-container-workflow-key-" + id.New()
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Code, key}
	var address atomic.Value
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-study", "browser_exec", map[string]any{
				"code": fmt.Sprintf("new_tab(%q)\nprint(js('document.body.textContent'))", address.Load().(string)+"/study"),
			})}}
		case 2:
			if !request.HasToolText("42 samples") {
				t.Error("isolated native reasoning lost its study observation")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-question", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which workflow figure should be reported?", "choices": []string{"Report the sample count", "Report the account label"}}},
			})}}
		case 3:
			if !request.HasToolText("Report the sample count") {
				t.Error("Commander did not deliver the workflow clarification answer")
			}
			return fakes.HermesResponse{Text: "The study measured 42 samples. Provider echo " + key, Escape: key}
		case 4:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-account", "browser_exec", map[string]any{
				"code": fmt.Sprintf("goto_url(%q)\nprint(js('document.body.textContent'))", address.Load().(string)+"/account"),
			})}}
		case 5, 7:
			if !request.HasToolText(label) || !request.HasToolText("42 samples") || !request.HasToolText("[REDACTED]") {
				t.Error("native reuse lost its authenticated account or protected observations")
			}
			return fakes.HermesResponse{Text: "Authenticated workflow reports 42 samples. Provider echo " + key, Escape: key}
		case 6:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("workflow-retained", "browser_exec", map[string]any{
				"code": "print(js('document.body.textContent'))",
			})}}
		default:
			t.Error("the isolated workflow exceeded its scripted model requests")
			return fakes.HermesResponse{Text: "Unexpected workflow continuation."}
		}
	}, protected[:3]...)
	endpoint := model.URL + "/chat/completions"
	secretName := modelSecretName(endpoint)
	login := &hermesLoginFixture{profile: site.Accounts[0].Profile}
	returned := make(chan action.Result, 4)
	h = newHermesSessionHostWithOptions(t, map[string]hermesSessionScenario{
		"prepare": {prepareOnly: true}, "login": {login: login, checkpointed: true},
	}, options, func(h *hermesSessionHost) action.Executor {
		fixture := newHermesBrowseFixture(h).(*hermesBrowseFixture)
		fixture.returned = returned
		return fixture
	})
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	defer stop()
	root := h.task(nil, sessions.Open, "prepare")
	h.call(root)
	binding := h.binding(root)
	config := hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: binding.Claim()}
	container := hermesContainerModelReference(t, ctx, api, config)
	// These synthetic values belong to the fixture website. The provider key
	// stays outside its bootstrap, files, environment and native task code.
	site.URL = api.exec(t, ctx, container, fixtures.HermesWorkflowSiteScript(site.Accounts[0].Username, site.Accounts[0].Password, site.Code, ""))["url"]
	if !strings.HasPrefix(site.URL, "http://127.0.0.1:") {
		t.Fatal("the integrated fixture did not use its worker's loopback namespace")
	}
	address.Store(site.URL)
	login.request = nativeLoginFixture(site, 0, "/login", "/account").request
	input := browsing.HermesBrowseInput{Objective: "Read the workflow study and report its sample count.", URL: site.URL + "/study",
		MaxIterations: 4, MaxTokensPerResponse: 512, TimeoutSeconds: 35}
	first := h.nativeBrowseTask(&root, input)
	firstBinding := h.binding(first)
	command := h.dispatch(first, &firstBinding)
	h.answerSetup(command, "browser_model_provider", "Other (OpenAI-compatible)")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	h.answerSetup(command, "browser_model_url", endpoint)
	if model.Calls() != 0 {
		t.Fatal("the isolated workflow contacted a model before managed key delivery")
	}
	lease := h.deliverModelKey(command, secretName, key, 1)
	started := h.checkpoint(controllerevents.CheckpointSave, "browse_started")
	if model.Calls() != 0 || h.snapshot(binding.Reference).State != "starting" {
		t.Fatal("isolated browse crossed its unconfirmed native-work fence")
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
				t.Fatal("isolated clarification was published before its checkpoint receipt")
			}
		}
	}
	h.replyCheckpoint(questionFence, h.persistCheckpoint(questionFence))
	h.answerSetup(command, "browser_clarification_1", "Report the sample count")
	final := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	firstOutput, ok := h.screenshotResult(command)
	<-returned
	requireHermesWorkflowBrowse(t, h, firstOutput, ok, binding, 3, protected...)
	assertHermesCheckpointFacts(t, h, command, firstBinding, append(protected, site.URL, input.Objective, "Which workflow figure should be reported?", "Report the sample count", "42 samples")...)
	if len(h.checkpoints[command.ID].Checkpoints) != 4 || model.Calls() != 3 {
		t.Fatal("integrated browse omitted its shared question/batch fences or repeated reasoning")
	}

	loginTask := h.task(&first, sessions.Open, "login")
	loginBinding := h.binding(loginTask)
	command = h.dispatch(loginTask, &loginBinding)
	started = h.checkpoint(controllerevents.CheckpointSave, "login_started")
	assertHermesWorkflowPosts(t, ctx, api, container, site.URL, "0", "0")
	if model.Calls() != 3 {
		t.Fatal("deterministic login performed model setup or reasoning")
	}
	h.replyCheckpoint(started, h.persistCheckpoint(started))
	h.loginCredentials(command, site, 0)
	codeRequest := h.loginCodeRequest(command, site)
	assertHermesWorkflowPosts(t, ctx, api, container, site.URL, "1", "0")
	if model.Calls() != 3 {
		t.Fatal("login credential delivery reached native model reasoning")
	}
	h.loginCode(codeRequest, site.Code, "")
	final = h.checkpoint(controllerevents.CheckpointSave, "login_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	loggedIn, ok := h.finish(command)
	if !ok {
		t.Fatal("isolated managed login did not complete")
	}
	requireNativeLogin(t, loggedIn, "verified", true)
	if loggedIn.Claim != binding.Claim() || len(h.checkpoints[command.ID].Checkpoints) != 2 {
		t.Fatal("isolated login changed its original claim or omitted recovery facts")
	}
	assertHermesCheckpointFacts(t, h, command, loginBinding, append(protected, site.URL, label)...)
	assertHermesWorkflowPosts(t, ctx, api, container, site.URL, "1", "1")

	waitHermesBrowseLease(t, lease)
	input.Objective, input.URL = "Inspect the retained signed-in workflow account.", site.URL+"/account"
	authenticated := h.nativeBrowseTask(&loginTask, input)
	authenticatedBinding := h.binding(authenticated)
	command = h.dispatch(authenticated, &authenticatedBinding)
	lease = h.deliverModelKey(command, secretName, key, 2)
	final = h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	authenticatedOutput, ok := h.screenshotResult(command)
	<-returned
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
	hermesScreenshotEvaluate(t, h, use, `(()=>{const echoes=document.querySelectorAll('p.secret');if(echoes.length!==3||document.querySelector('h1')?.textContent!=='Signed-in workflow account')throw new Error('invalid_workflow_reference');for(const element of echoes)element.textContent='Credential echo [REDACTED]';return true})()`)
	if redactedPixels, _ := captureHermesScreenshot(t, h, authenticated, "desktop"); firstPixels != redactedPixels {
		t.Fatal("integrated screenshot pixels disclosed managed credentials or the transient code")
	}
	if model.Calls() != beforeCapture {
		t.Fatal("isolated screenshot capture invoked a model")
	}
	for _, message := range h.history[events:] {
		if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
			if _, answered := h.accepted[message.ID]; !answered {
				t.Fatal("isolated screenshot asked for setup or managed credentials")
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
	if _, ok := h.screenshotResult(command); ok || model.Calls() != 7 {
		t.Fatal("isolated browse accepted output without its final persistence receipt")
	}
	suppressed := <-returned
	if len(suppressed.Output) != 0 || len(suppressed.Artifacts) != 0 || suppressed.Session == nil || suppressed.Session.Reference != binding.Reference || h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("failed isolated final persistence exposed prepared output or lost browser ownership")
	}
	assertHermesCheckpointFacts(t, h, command, refusedBinding, append(protected, site.URL, input.Objective, label)...)
	assertHermesWorkflowPosts(t, ctx, api, container, site.URL, "1", "1")
	assertNativeLoginPrivate(t, h, protected...)

	// An independent root with an old saved start fence must fail before model
	// setup and before a container is allocated under its fresh creation claim.
	replay := h.nativeBrowseTask(nil, input)
	replayBinding := h.binding(replay)
	beforeReplay := len(h.history)
	command = h.dispatch(replay, &replayBinding)
	load := h.checkpoint(controllerevents.CheckpointLoad, "browse_started")
	record := h.checkpoints[command.ID]
	_, err := record.RecordCheckpoint("browse_started", json.RawMessage(`{"version":2,"phase":"browser_work_pending"}`))
	hermesSessionCheck(t, err)
	h.checkpoints[command.ID] = record
	h.replyCheckpoint(load, h.persistCheckpoint(load))
	assertHermesCheckpointFailure(t, h, command)
	<-returned
	if model.Calls() != 7 || len(hermesContainerModelReferences(t, ctx, api, hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: replayBinding.Claim()})) != 0 {
		t.Fatal("saved-fence rejection started model work or an isolated browser")
	}
	for _, message := range h.history[beforeReplay:] {
		if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
			if _, answered := h.accepted[message.ID]; !answered {
				t.Fatal("saved-fence rejection entered model setup or secret delivery")
			}
		}
	}
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" || len(hermesContainerModelReferences(t, ctx, api, config)) != 0 {
		t.Fatal("the integrated isolated workflow did not confirm session and container closure")
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
			t.Fatal("a managed value entered an isolated browse artifact")
		}
	}
}

func assertHermesWorkflowPosts(t *testing.T, ctx context.Context, api hermesDockerProbe, container, site, password, code string) {
	t.Helper()
	stats := api.exec(t, ctx, container, fixtures.HermesWorkflowStatsScript(site))
	if stats["password_posts"] != password || stats["code_posts"] != code {
		t.Fatal("the isolated workflow crossed an unconfirmed login fence or repeated credential/code submission")
	}
}
