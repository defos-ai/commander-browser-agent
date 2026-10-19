//go:build integration

package browser_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Run only against a disposable Compose project with ENV=tests: cleanup removes
// all public operation data in that project. Docker lifecycle belongs to CI/setup.
func TestAgentThroughAPI(t *testing.T) {
	a := connectAPI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	a.request(t, ctx, http.MethodPost, "/api/v1/testing/cleanup", nil, http.StatusNoContent, nil)
	await(t, ctx, func() bool {
		var agents agentList
		a.request(t, ctx, http.MethodGet, "/api/v1/agents", nil, http.StatusOK, &agents)
		for _, agent := range agents.Items {
			if agent.Type != "browser-agent" || agent.Status != "ready" || agent.Stale {
				continue
			}
			ready := map[string]bool{}
			for _, action := range agent.Actions {
				ready[action.Name] = action.Status == "ready"
			}
			if ready["capture-page"] && ready["check-accessibility"] {
				return true
			}
		}
		return false
	})

	// Choose the ID before sending so cleanup can find work even if the response
	// is lost after the controller accepts the operation.
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatal(err)
	}
	idBytes[6], idBytes[8] = idBytes[6]&0x0f|0x40, idBytes[8]&0x3f|0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:])
	base := "/api/v1/operations/" + id
	t.Cleanup(func() {
		if err := a.cleanup(id); err != nil {
			t.Errorf("clean up browser operation: %v", err)
		}
	})
	input := map[string]any{"url": "http://fixture/"}
	var operation operationResponse
	a.request(t, ctx, http.MethodPost, "/api/v1/operations", map[string]any{
		"operation_id": id, "mission_name": "review-page", "input": input,
	}, http.StatusCreated, &operation)
	for _, action := range []string{"capture-page", "check-accessibility"} {
		a.request(t, ctx, http.MethodPost, base+"/tasks", map[string]any{
			"action_name": action, "input": input,
		}, http.StatusCreated, nil)
	}
	await(t, ctx, func() bool {
		a.request(t, ctx, http.MethodGet, base, nil, http.StatusOK, &operation)
		done := len(operation.Tasks) == 2
		for _, task := range operation.Tasks {
			if task.Status == "failed" || task.Status == "cancelled" {
				t.Fatalf("%s task %s: %s", task.ActionName, task.Status, task.Error)
			}
			done = done && task.Status == "succeeded"
		}
		return done
	})
	var capture taskResponse
	for _, task := range operation.Tasks {
		if !strings.HasPrefix(task.AgentID, "browser-agent-") {
			t.Fatalf("task was not assigned to a browser agent: %+v", task)
		}
		if task.ActionName == "capture-page" {
			capture = task
		}
	}
	if capture.Result == nil || len(capture.Result.Artifacts) != 1 || len(capture.Result.Artifacts[0].Files) != 2 {
		t.Fatalf("capture-page produced no desktop/mobile artifacts: %+v", capture.Result)
	}
	artifact := capture.Result.Artifacts[0]
	dimensions := map[string][2]int{"desktop.png": {1280, 800}, "mobile.png": {390, 844}}
	for _, file := range artifact.Files {
		want, ok := dimensions[file.Name]
		if !ok {
			t.Fatalf("unexpected or duplicate screenshot %q", file.Name)
		}
		delete(dimensions, file.Name)
		path := "/api/v1/artifacts/" + artifact.ID + "/files/" + file.ID
		content := a.request(t, ctx, http.MethodGet, path, nil, http.StatusOK, nil)
		digest := sha256.Sum256(content)
		picture, err := png.Decode(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("decode %s as PNG: %v", file.Name, err)
		}
		if picture.Bounds().Dx() != want[0] || picture.Bounds().Dy() != want[1] || file.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("screenshot dimensions or checksum differ: file=%+v bounds=%v", file, picture.Bounds())
		}
	}

	// The client reviews results and completes the operation; agents never do so.
	a.request(t, ctx, http.MethodPost, base+"/complete", map[string]any{
		"output": map[string]any{"summary": "Captured desktop/mobile screenshots and checked rendered accessibility."},
	}, http.StatusOK, &operation)
	if operation.Status != "completed" || operation.ReportID == "" {
		t.Fatalf("operation has no completed report: %+v", operation)
	}
	a.request(t, ctx, http.MethodGet, base+"/report", nil, http.StatusOK, nil)
}

// Keep the agent independent of Commander application modules. These small
// response types describe only the public API fields the scenario needs.
type operationResponse struct {
	Status   string         `json:"status"`
	ReportID string         `json:"report_id"`
	Tasks    []taskResponse `json:"tasks"`
}

type taskResponse struct {
	ID           string `json:"id"`
	ActionName   string `json:"action_name"`
	AgentID      string `json:"agent_id"`
	ParentTaskID string `json:"parent_task_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	Result       *struct {
		Artifacts []struct {
			ID    string `json:"id"`
			Files []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		} `json:"artifacts"`
	} `json:"result"`
}

type agentList struct {
	Items []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Stale   bool   `json:"stale"`
		Actions []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"actions"`
		Work *struct {
			OperationID string `json:"operation_id"`
		} `json:"work"`
	} `json:"items"`
}

type apiClient struct {
	url, username, password string
	client                  *http.Client
}

func connectAPI(t *testing.T) *apiClient {
	t.Helper()
	a := &apiClient{
		url: os.Getenv("COMMANDER_API_URL"), username: os.Getenv("API_USERNAME"), password: os.Getenv("API_PASSWORD"),
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
	if a.url == "" {
		a.url = "http://127.0.0.1:4284"
	}
	a.url = strings.TrimRight(a.url, "/")
	if a.username == "" || a.password == "" {
		t.Fatal("integration tests require API_USERNAME and API_PASSWORD for a disposable stack started with ENV=tests")
	}
	t.Cleanup(a.client.CloseIdleConnections)
	return a
}

func (a *apiClient) request(t *testing.T, ctx context.Context, method, path string, body any, status int, out any) []byte {
	t.Helper()
	data, got, err := a.send(ctx, method, path, body)
	if err != nil || got != status {
		t.Fatalf("%s %s: status=%d, want %d, error=%v body=%s", method, path, got, status, err, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return data
}

func (a *apiClient) send(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, a.url+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	request.SetBasicAuth(a.username, a.password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 2<<20))
	return data, response.StatusCode, err
}

func await(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	for !ready() {
		if err := pause(ctx); err != nil {
			t.Fatalf("browser agent did not reach expected state: %v", err)
		}
	}
}

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

func (a *apiClient) cleanup(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := "/api/v1/operations/" + id
	_, status, err := a.send(ctx, http.MethodPost, base+"/cancel", nil)
	if err != nil || (status != http.StatusOK && status != http.StatusConflict && status != http.StatusNotFound) {
		return fmt.Errorf("cancel operation: status=%d error=%v", status, err)
	}
	for {
		data, status, err := a.send(ctx, http.MethodGet, base, nil)
		if err != nil {
			return err
		}
		var operation operationResponse
		if status != http.StatusNotFound && (status != http.StatusOK || json.Unmarshal(data, &operation) != nil) {
			return fmt.Errorf("read operation: status=%d body=%s", status, data)
		}
		busy := status != http.StatusNotFound && operation.Status != "completed" && operation.Status != "cancelled" && operation.Status != "failed"
		for _, task := range operation.Tasks {
			busy = busy || task.Status == "queued" || task.Status == "running" || task.Status == "interrupted"
		}
		data, status, err = a.send(ctx, http.MethodGet, "/api/v1/agents", nil)
		var agents agentList
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &agents) != nil {
			return fmt.Errorf("read agents: status=%d error=%v", status, err)
		}
		for _, agent := range agents.Items {
			busy = busy || agent.Work != nil && agent.Work.OperationID == id
		}
		if !busy {
			data, status, err = a.send(ctx, http.MethodPost, "/api/v1/testing/cleanup", nil)
			if err != nil || (status != http.StatusNoContent && status != http.StatusConflict) {
				return fmt.Errorf("clear test state: status=%d error=%v body=%s", status, err, data)
			}
			if status == http.StatusNoContent {
				_, status, err = a.send(ctx, http.MethodGet, base, nil)
				if err != nil || status != http.StatusNotFound {
					return fmt.Errorf("operation remained after cleanup: status=%d error=%v", status, err)
				}
				return nil
			}
		}
		if err := pause(ctx); err != nil {
			return fmt.Errorf("work did not settle for cleanup: %w", err)
		}
	}
}
