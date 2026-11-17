//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func (h *hermesSessionHost) setupMessage(command messages.Message) messages.Message {
	h.t.Helper()
	return h.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested ||
			message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
	})
}

func (h *hermesSessionHost) answerQuestion(command messages.Message, key, answer string) {
	h.t.Helper()
	message := h.setupMessage(command)
	if message.Type != controllerevents.QuestionRequested {
		h.t.Fatal("execution did not ask its non-secret question")
	}
	request, err := controllerevents.DecodeQuestionRequest(message)
	hermesSessionCheck(h.t, err)
	if request.CommandID != command.ID || request.Key != key || len(request.Options) > 4 {
		h.t.Fatal("question lost its bounded identity")
	}
	now := time.Now().UTC()
	question := operations.Question{ID: id.New(), TaskID: message.TaskID, CommandID: command.ID, Key: request.Key, Text: request.Text, Options: request.Options,
		Status: operations.QuestionAnswered, Answer: answer, AnsweredBy: operations.AnswerOperator, AskedAt: now, FallbackAt: now.Add(time.Minute), AnsweredAt: &now}
	response, err := agentevents.NewQuestionResponse(message, agentevents.QuestionResponse{CommandID: command.ID, Key: request.Key, Question: &question})
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = response
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), response))
}

func (h *hermesSessionHost) deliverModelSecret(command messages.Message, name, key string, version uint64) time.Time {
	h.t.Helper()
	return h.deliverReusableSecret(command, name, key, version, nil)
}

func (h *hermesSessionHost) deliverReusableSecret(command messages.Message, name, key string, version uint64, expectedDefault []byte) time.Time {
	h.t.Helper()
	message := h.setupMessage(command)
	if message.Type != controllerevents.SecretSyncRequested {
		h.t.Fatal("setup asked a question or ended before managed secret delivery")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	hermesSessionCheck(h.t, err)
	defer clear(request.Default)
	if request.Request.AgentID != h.host.ID() || request.Request.AgentType != "browser-agent" {
		h.t.Fatal("model setup secret request lost its exact agent identity")
	}
	if request.Request.CommandID != "" || request.Request.OperationID != "" || request.Request.TaskID != "" {
		h.t.Fatal("reusable model setup request unexpectedly used transient execution identity")
	}
	if request.Request.Key.Name != name {
		h.t.Fatal("model setup secret request did not use its canonical managed name")
	}
	if request.Request.EffectiveType() != secrets.SecretReusable {
		h.t.Fatal("model setup secret request was not reusable")
	}
	if !bytes.Equal(request.Default, expectedDefault) || (request.Default == nil) != (expectedDefault == nil) {
		h.t.Fatal("managed request lost its exact selected default or seeded credentials")
	}
	if expectedDefault == nil && bytes.Contains(message.Payload, []byte(key)) {
		h.t.Fatal("a model setup value appeared before private delivery")
	}
	now := time.Now().UTC()
	value := []byte(key)
	defer clear(value)
	if h.modelKeyMetadata == nil {
		h.modelKeyMetadata = make(map[string]secrets.Metadata)
	}
	metadata, exists := h.modelKeyMetadata[name]
	if !exists {
		metadata = secrets.Metadata{ID: id.New(), Key: request.Request.Key, CreatedAt: now}
	}
	metadata.Version, metadata.UpdatedAt = version, now
	h.modelKeyMetadata[name] = metadata
	delivery, err := agentevents.NewSecretDelivery(request.Request, metadata, value)
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = delivery
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), delivery))
	return delivery.ExpiresAt
}

func (h *hermesSessionHost) answerModelConfig(command messages.Message, provider, model, endpoint string) {
	h.t.Helper()
	h.answerQuestion(command, "browser_model_provider_1", provider)
	h.answerQuestion(command, "browser_model_1", model)
	if provider == "other" {
		h.answerQuestion(command, "browser_model_url_1", endpoint)
	}
}

func (h *hermesSessionHost) rememberModelConfig(command messages.Message, provider, model, endpoint string) {
	h.t.Helper()
	h.deliverReusableSecret(command, commander.ModelProviderSecretName, provider, 1, []byte(provider))
	if provider == "other" {
		h.deliverReusableSecret(command, commander.ModelURLSecretName, endpoint, 1, []byte(endpoint))
	}
	h.deliverReusableSecret(command, commander.ModelNameSecretName, model, 1, []byte(model))
}

func TestHermesManagedModelSetupReusesSelectionAndProtectsRotation(t *testing.T) {
	firstKey, secondKey := "fixture-managed-first-"+id.New(), "fixture-managed-second-"+id.New()
	ignored := "fixture-ignored-environment-" + id.New()
	t.Setenv("OPENAI_API_KEY", ignored)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>Managed fixture observation "+firstKey+"</p>")
	}))
	t.Cleanup(site.Close)
	model := fakes.NewHermesModel(t, firstKey, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call == 1 || call == 3 {
			code := "# Inspect the retained managed fixture\nprint(js('document.body.textContent'))"
			if call == 1 {
				code = fmt.Sprintf("# Open the managed fixture\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL)
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall(fmt.Sprintf("managed-%d", call), "browser_exec", map[string]any{"code": code})}}
		}
		if call != 2 && call != 4 || !request.HasToolText("Managed fixture observation") {
			t.Error("managed model rotation lost the retained browser observation")
		}
		return fakes.HermesResponse{Text: "Managed fixture completed."}
	}, ignored)
	endpoint := model.URL + "/chat/completions"
	name := fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
	profilesBefore := hermesSessionProfiles(t)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"managed": {managed: true}})
	t.Setenv("BROWSER_MODEL_PROVIDER", "other")
	t.Setenv("BROWSER_MODEL_NAME", "gpt-4.1-mini")
	root := h.task(nil, sessions.Open, "managed")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	rootCommandID := command.ID
	h.answerQuestion(command, "browser_model_url_1", endpoint)
	if model.Calls() != 0 {
		t.Fatal("model requests started before managed credentials arrived")
	}
	lease := h.deliverModelSecret(command, name, firstKey, 1)
	h.rememberModelConfig(command, "other", "gpt-4.1-mini", endpoint)
	if model.ValidationCalls() != 1 {
		t.Fatal("settings were remembered before a model-only validation")
	}
	first, ok := h.finish(command)
	if !ok || first.Result.Status != "completed" || model.Calls() != 2 {
		t.Fatal("managed setup did not reach the real provider through its selected key")
	}
	// Commander owns reusable delivery leases. Rotation is observed after the
	// previous delivery expires, rather than bypassing its legitimate cache.
	wait := time.NewTimer(max(time.Until(lease), 0) + 100*time.Millisecond)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-t.Context().Done():
		t.Fatal("managed fixture lease wait was cancelled")
	}
	model.RotateKey(secondKey)
	// A retained session keeps its validated model even after host defaults change.
	t.Setenv("BROWSER_MODEL_PROVIDER", "openai")
	t.Setenv("BROWSER_MODEL_NAME", "changed-model")
	child := h.task(&root, sessions.Open, "managed")
	childBinding := h.binding(child)
	command = h.dispatch(child, &childBinding)
	h.deliverModelSecret(command, name, secondKey, 2)
	second, ok := h.finish(command)
	if !ok || second.Result.Status != "completed" || second.Claim != first.Claim || model.Calls() != 4 || model.ValidationCalls() != 1 {
		t.Fatal("reuse repeated setup, cached a plaintext key or replaced browser ownership")
	}
	for _, output := range []hermesSessionOutput{first, second} {
		encoded, _ := json.Marshal(output)
		for _, key := range []string{firstKey, secondKey, ignored} {
			if bytes.Contains(encoded, []byte(key)) {
				t.Error("managed model values escaped through an action result")
			}
		}
	}
	assertHermesOwnedFilesAndEnvironmentPrivate(t, profilesBefore, firstKey, secondKey, ignored)
	for _, message := range h.history {
		if message.Type == controllerevents.QuestionRequested {
			request, err := controllerevents.DecodeQuestionRequest(message)
			hermesSessionCheck(t, err)
			if request.CommandID != rootCommandID || request.Key != "browser_model_url_1" {
				h.t.Fatal("retained model setup repeated a question")
			}
		}
	}
}

func TestHermesFreshHostsValidateBeforeSeedingMatchingManagedDefaults(t *testing.T) {
	key := "fixture-canonical-openai-" + id.New()
	t.Setenv("OPENAI_API_KEY", "ignored-openai-environment-"+id.New())
	previousHost := ""
	for range 2 {
		h := newHermesSessionHost(t, map[string]hermesSessionScenario{"prepare": {managed: true, prepareOnly: true}})
		if h.host.ID() == previousHost {
			t.Fatal("shared configuration fixture reused the same agent instance")
		}
		previousHost = h.host.ID()
		var validations atomic.Int32
		// Keep canonical OpenAI key routing offline. The default-catalog workflow
		// separately exercises the real HTTP validator against a local provider.
		h.setup.ValidateModel = func(_ context.Context, model contracts.ModelConfig) error {
			validations.Add(1)
			if model.Provider != "openai" || model.Model != "chat-latest" || model.Endpoint != "https://api.openai.com/v1/chat/completions" || string(model.APIKey) != key {
				t.Error("model validation lost its exact settings or canonical key")
			}
			return nil
		}
		root := h.task(nil, sessions.Open, "prepare")
		binding := h.binding(root)
		command := h.dispatch(root, &binding)
		h.answerModelConfig(command, "openai", "chat-latest", "")
		if validations.Load() != 0 {
			t.Fatal("model validation started before managed key delivery")
		}
		h.deliverModelSecret(command, "OPENAI_API_KEY", key, 1)
		h.rememberModelConfig(command, "openai", "chat-latest", "")
		output, ok := h.finish(command)
		if !ok || validations.Load() != 1 || output.Session.State != "starting" || output.Result.ModelRequests != 0 {
			t.Fatal("setup did not validate once before matching existing defaults, or opened a browser")
		}
		questions := map[string]bool{}
		for _, message := range h.history {
			if message.Type == controllerevents.QuestionRequested {
				request, err := controllerevents.DecodeQuestionRequest(message)
				hermesSessionCheck(t, err)
				questions[request.Key] = true
			}
		}
		if len(questions) != 2 || !questions["browser_model_provider_1"] || !questions["browser_model_1"] {
			t.Fatal("a fresh host did not ask its two non-secret model questions")
		}
	}
}

// Inspect only the private synthetic worker homes created by this test. Exact
// values are never printed, and no unrelated host home or process is inspected.
func assertHermesOwnedFilesAndEnvironmentPrivate(t *testing.T, before map[string]bool, forbidden ...string) {
	t.Helper()
	var owned []string
	for path := range hermesSessionProfiles(t) {
		if !before[path] {
			owned = append(owned, path)
		}
	}
	if len(owned) == 0 {
		t.Fatal("the privacy fixture has no owned native runtime")
	}
	for _, root := range owned {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
				_ = file.Close()
				if err != nil || len(data) > 8<<20 {
					return errorsFixturePrivacy
				}
				for _, value := range forbidden {
					if bytes.Contains(data, []byte(value)) {
						t.Error("a managed provider value entered a private worker file")
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal("the owned native files could not be checked within fixture bounds")
		}
	}
	processes, _ := filepath.Glob("/proc/[0-9]*/cwd")
	seenPython, seenBrowser := false, false
	for _, cwd := range processes {
		directory, err := os.Readlink(cwd)
		if err != nil {
			continue
		}
		ours := false
		for _, root := range owned {
			ours = ours || directory == root || strings.HasPrefix(directory, root+string(os.PathSeparator))
		}
		if !ours {
			continue
		}
		process := filepath.Dir(cwd)
		comm, _ := os.ReadFile(filepath.Join(process, "comm"))
		environment, err := os.ReadFile(filepath.Join(process, "environ"))
		if err != nil {
			continue
		}
		seenPython = seenPython || bytes.HasPrefix(comm, []byte("python"))
		seenBrowser = seenBrowser || bytes.HasPrefix(comm, []byte("lightpanda"))
		for _, value := range forbidden {
			if bytes.Contains(environment, []byte(value)) {
				t.Error("a managed provider value entered an owned child environment")
			}
		}
	}
	if !seenPython || !seenBrowser {
		t.Fatal("the privacy fixture did not inspect its Python and Lightpanda children")
	}
}

var errorsFixturePrivacy = fmt.Errorf("bounded native privacy fixture failure")
