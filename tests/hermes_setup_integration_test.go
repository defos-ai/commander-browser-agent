//go:build hermesintegration

package browser_test

import (
	"bytes"
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
	"testing"
	"time"

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

func (h *hermesSessionHost) answerSetup(command messages.Message, key, answer string) {
	h.t.Helper()
	message := h.setupMessage(command)
	if message.Type != controllerevents.QuestionRequested {
		h.t.Fatal("managed setup did not ask its non-secret question first")
	}
	request, err := controllerevents.DecodeQuestionRequest(message)
	hermesSessionCheck(h.t, err)
	if request.CommandID != command.ID || request.Key != key || len(request.Options) > 4 {
		h.t.Fatal("managed setup lost its bounded question order or identity")
	}
	now := time.Now().UTC()
	question := operations.Question{ID: id.New(), TaskID: message.TaskID, CommandID: command.ID, Key: request.Key, Text: request.Text, Options: request.Options,
		Status: operations.QuestionAnswered, Answer: answer, AnsweredBy: operations.AnswerOperator, AskedAt: now, FallbackAt: now.Add(time.Minute), AnsweredAt: &now}
	response, err := agentevents.NewQuestionResponse(message, agentevents.QuestionResponse{CommandID: command.ID, Key: request.Key, Question: &question})
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = response
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), response))
}

func (h *hermesSessionHost) deliverModelKey(command messages.Message, name, key string, version uint64) time.Time {
	h.t.Helper()
	message := h.setupMessage(command)
	if message.Type != controllerevents.SecretSyncRequested {
		h.t.Fatal("setup repeated questions or ended before managed key delivery")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	hermesSessionCheck(h.t, err)
	defer clear(request.Default)
	if request.Request.AgentID != h.host.ID() || request.Request.AgentType != "browser-agent" {
		h.t.Fatal("model key request lost its exact agent identity")
	}
	if request.Request.CommandID != "" || request.Request.OperationID != "" || request.Request.TaskID != "" {
		h.t.Fatal("reusable model key request unexpectedly used transient execution identity")
	}
	if request.Request.Key.Name != name {
		h.t.Fatal("model key request did not use its canonical managed name")
	}
	if request.Request.EffectiveType() != secrets.SecretReusable {
		h.t.Fatal("model key request was not reusable")
	}
	if request.Default != nil {
		h.t.Fatal("model key request unexpectedly supplied a default seed")
	}
	if bytes.Contains(message.Payload, []byte(key)) {
		h.t.Fatal("the model key value appeared before private delivery")
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
	root := h.task(nil, sessions.Open, "managed")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	h.answerSetup(command, "browser_model_provider", "Other (OpenAI-compatible)")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	h.answerSetup(command, "browser_model_url", endpoint)
	if model.Calls() != 0 {
		t.Fatal("model requests started before managed credentials arrived")
	}
	lease := h.deliverModelKey(command, name, firstKey, 1)
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
	child := h.task(&root, sessions.Open, "managed")
	childBinding := h.binding(child)
	command = h.dispatch(child, &childBinding)
	h.deliverModelKey(command, name, secondKey, 2)
	second, ok := h.finish(command)
	if !ok || second.Result.Status != "completed" || second.Claim != first.Claim || model.Calls() != 4 {
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
}

func TestHermesSetupRequestsCanonicalOpenAIKeyWithoutModelAccess(t *testing.T) {
	key := "fixture-canonical-openai-" + id.New()
	t.Setenv("OPENAI_API_KEY", "ignored-openai-environment-"+id.New())
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"prepare": {managed: true, prepareOnly: true}})
	root := h.task(nil, sessions.Open, "prepare")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	h.answerSetup(command, "browser_model_provider", "OpenAI")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	h.deliverModelKey(command, "OPENAI_API_KEY", key, 1)
	output, ok := h.finish(command)
	if !ok || output.Session.State != "starting" || output.Result.ModelRequests != 0 {
		t.Fatal("preparation-only setup opened a browser or model request")
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
