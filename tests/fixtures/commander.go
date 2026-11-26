//go:build integration

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/host"
	"github.com/defos-ai/commander/framework/id"
)

// Commander covers the public secret and session operations outside missiontest.
type Commander struct {
	t           *testing.T
	baseURL     string
	credentials host.BasicCredentials
	client      http.Client
}

func NewCommander(t *testing.T, apiURL string, credentials host.BasicCredentials) *Commander {
	return &Commander{t: t, baseURL: strings.TrimRight(apiURL, "/"), credentials: credentials,
		client: http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Commander) SetSecret(name, value string) {
	c.t.Helper()
	c.request(c.t.Context(), http.MethodPost, "/api/v1/secrets", map[string]string{"name": name, "value": value}, http.StatusCreated, nil)
}

func (c *Commander) DeliverOTP(task operations.Task, name, value string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pending struct {
			Items []struct {
				secrets.Request
				Name string `json:"name"`
			} `json:"items"`
		}
		c.request(ctx, http.MethodGet, "/api/v1/secret-requests?type=transient&agent_id="+url.QueryEscape(task.AgentID), nil, http.StatusOK, &pending)
		for _, r := range pending.Items {
			if r.OperationID == task.OperationID && r.TaskID == task.ID && r.CommandID == task.CommandID &&
				r.AgentID == task.AgentID && r.Type == secrets.SecretTransient && r.Status == secrets.Pending && r.Name == name && id.Valid(r.ID) {
				c.request(ctx, http.MethodPost, "/api/v1/secret-requests/"+r.ID+"/values", map[string]string{"value": value}, http.StatusAccepted, nil)
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			c.t.Fatalf("login task %s did not request its transient code: %v", task.ID, ctx.Err())
		}
	}
}

func (c *Commander) CloseSession(task operations.Task) {
	c.t.Helper()
	if task.Result == nil || task.Result.Session == nil {
		c.t.Fatal("expected a browser session to close")
	}
	ref := task.Result.Session.Reference
	var closed sessions.Session
	c.request(c.t.Context(), http.MethodPost, "/api/v1/sessions/control",
		sessions.Control{Reference: ref, Method: "close", RequestID: id.New()}, http.StatusOK, &closed)
	if closed.Reference != ref || closed.State != "closed" {
		c.t.Fatalf("expected confirmed close of session %s, got %s on %s", ref.ID, closed.State, closed.ID)
	}
}

func (c *Commander) request(ctx context.Context, method, path string, input any, status int, output any) {
	c.t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			c.t.Fatal("could not encode test API request")
		}
		defer clear(data)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		c.t.Fatal("invalid test API request")
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.credentials.Username, c.credentials.Password)
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s returned HTTP %d, expected %d", method, path, resp.StatusCode, status)
	}
	if output != nil && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output) != nil {
		c.t.Fatalf("%s %s returned invalid JSON", method, path)
	}
}
