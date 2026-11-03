package actions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/browser/lightpanda"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/artifacts"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

const BrowseName = "browse"
const BrowseDescription = "Navigate, interact with and extract structured findings from websites using the browser agent's own model, non-secret clarification questions and an isolated Lightpanda session"

// BrowserRunner is the shared host-owned browser session runtime. Action
// constructors accept the production dependency without creating another owner.
type BrowserRunner interface {
	Run(context.Context, browserruntime.Options, func(context.Context) error) (sessions.Session, error)
}

type browserSetupRunner interface {
	WithSession(context.Context, func(context.Context) error) error
}

type BrowseInput struct {
	Objective           string   `json:"objective"`
	URL                 string   `json:"url"`
	SessionScope        string   `json:"session_scope"`
	SessionID           string   `json:"session_id,omitempty"`
	AllowedHosts        []string `json:"allowed_hosts,omitempty"`
	MaxSteps            int      `json:"max_steps,omitempty"`
	TimeoutSeconds      int      `json:"timeout_seconds,omitempty"`
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
}

type BrowseOutput struct {
	Engine browserruntime.Engine `json:"engine"`
	intelligence.Result
}

type Browse struct {
	*action.Base
	runner         BrowserRunner
	configuration  atomic.Pointer[browseConfiguration]
	validateInput  func(any) error
	validateOutput func(any) error
	setup          browsing.Setup
}

type browseConfiguration struct {
	runtime lightpanda.Runtime
}

// NewBrowse prepares a fresh action without discovery, credentials or I/O.
func NewBrowse(runner BrowserRunner) *Browse {
	input, err := action.ResolveSchema(json.RawMessage(browseInputSchema))
	if err != nil {
		panic("invalid built-in browse input schema: " + err.Error())
	}
	output, err := action.ResolveSchema(json.RawMessage(browseOutputSchema))
	if err != nil {
		panic("invalid built-in browse output schema: " + err.Error())
	}
	return &Browse{
		Base: action.NewBase(actionmodel.Action{
			Name: BrowseName, Description: BrowseDescription, SessionMode: "create",
			InputSchema: json.RawMessage(browseInputSchema), OutputSchema: json.RawMessage(browseOutputSchema),
		}),
		runner: runner, validateInput: input.Validate, validateOutput: output.Validate,
	}
}

func (a *Browse) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (status action.Status, discoveryErr error) {
	a.configuration.Store(nil)
	a.SetStatus(action.Unknown)
	defer func() {
		if err := ctx.Err(); err != nil {
			a.configuration.Store(nil)
			a.SetStatus(action.Unknown)
			status, discoveryErr = action.Unknown, err
		}
	}()
	if env == nil {
		env = &environment.Local{}
	}
	if a.runner == nil {
		a.SetUnavailable("The host must provide its browser session runner", actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: "browser-session-provider"})
		return action.Unavailable, nil
	}
	runtime, err := lightpanda.Discover(ctx, env, scope)
	if err != nil {
		var unavailable *lightpanda.UnavailableError
		if errors.As(err, &unavailable) {
			a.SetUnavailable(unavailable.Reason, unavailable.Requirement)
			return action.Unavailable, nil
		}
		return action.Unknown, err
	}
	if err := ctx.Err(); err != nil {
		return action.Unknown, err
	}
	a.configuration.Store(&browseConfiguration{runtime: runtime})
	a.SetStatus(action.Ready)
	return action.Ready, nil
}

func (a *Browse) Run(ctx context.Context, raw json.RawMessage) (result action.Result, runErr error) {
	if err := ctx.Err(); err != nil {
		return action.Result{}, err
	}
	if err := a.Check(); err != nil {
		return action.Result{}, err
	}
	configuration := a.configuration.Load()
	if configuration == nil || a.runner == nil {
		return action.Result{}, errors.New("browse is unavailable; run discovery first")
	}
	if len(raw) > 128*1024 {
		return action.Result{}, errors.New("browse input exceeds its size limit")
	}
	if err := validateBrowseJSON(a.validateInput, raw); err != nil {
		return action.Result{}, errors.New("browse input does not match its schema")
	}
	input := BrowseInput{MaxSteps: 12, TimeoutSeconds: 120, MaxCompletionTokens: 8192}
	if err := action.DecodeInput(raw, &input); err != nil {
		return action.Result{}, errors.New("invalid browse input")
	}
	if err := input.validate(); err != nil {
		return action.Result{}, err
	}
	// Operator-first setup has its own bound. It must not consume the active
	// browsing allowance, and it never extends the controller's task deadline.
	setupCtx, stopSetup := context.WithTimeout(ctx, 10*time.Minute)
	var modelConfiguration intelligence.ModelConfig
	var key []byte
	var checkpoints *browsing.Checkpoints
	prepare := func(ctx context.Context) error {
		var err error
		checkpoints, err = browsing.NewCheckpoints(ctx, BrowseName)
		if err != nil {
			return err
		}
		modelConfiguration, key, err = a.setup.Prepare(ctx, input.SessionScope, input.SessionID)
		return err
	}
	var err error
	if runner, ok := a.runner.(browserSetupRunner); ok {
		err = runner.WithSession(setupCtx, prepare)
	} else {
		err = prepare(setupCtx)
	}
	if err == nil {
		err = setupCtx.Err()
	}
	stopSetup()
	defer clear(key)
	if err != nil {
		return action.Result{}, err
	}
	budget := browsing.NewBudget(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer budget.Close()
	var findings intelligence.Result
	defer func() {
		status := "failed"
		if runErr == nil && len(result.Output) != 0 {
			status = findings.Status
		}
		progress := intelligence.ProgressSnapshot(findings)
		finish := func(ctx context.Context) error {
			return checkpoints.Finish(ctx, status, &progress, false)
		}
		var err error
		if runner, ok := a.runner.(browserSetupRunner); ok {
			err = runner.WithSession(budget.Context(), finish)
		} else {
			err = finish(budget.Context())
		}
		if budgetErr := budget.Err(); budgetErr != nil {
			err = budgetErr
		}
		if err != nil {
			result = action.Result{Session: result.Session}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				err = errors.New("cannot finalize browser progress; inspect the session before retrying")
			}
			runErr = errors.Join(runErr, err)
		}
	}()
	if len(key) == 0 || len(key) > 4096 || strings.ContainsAny(string(key), "\r\n") {
		return action.Result{}, errors.New("browser model credentials are invalid")
	}
	credential := string(key)
	redact := func(text string) string { return strings.ReplaceAll(text, credential, "[REDACTED]") }
	if err := checkBrowseURLCredential(input.URL, credential); err != nil {
		return action.Result{}, err
	}
	model, err := intelligence.NewModel(modelConfiguration, key)
	if err != nil {
		return action.Result{}, errors.New("cannot configure the browser model client")
	}
	if closer, ok := model.(io.Closer); ok {
		defer closer.Close()
	}
	request := intelligence.Request{
		Objective: redact(input.Objective), URL: input.URL, AllowedHosts: slices.Clone(input.AllowedHosts),
		MaxSteps: input.MaxSteps, MaxCompletionTokens: input.MaxCompletionTokens,
	}
	hooks := intelligence.Hooks{
		Redact:     redact,
		Log:        func(ctx context.Context, message string) error { return action.Log(ctx, redact(message)) },
		Checkpoint: checkpoints.Progress,
		Question: func(ctx context.Context, key, text string, options []string) (string, error) {
			budget.Pause()
			defer budget.Resume()
			waiting, stop := context.WithTimeout(ctx, 3*time.Minute)
			defer stop()
			const notice = "Browser clarification (non-secret). Do not enter passwords, credentials, authentication codes or API keys.\n\n"
			question, err := action.Question(waiting, key, notice+redact(text), options...)
			if err != nil {
				return "", err
			}
			return question.Answer, nil
		},
	}
	snapshot, runErr := a.runner.Run(budget.Context(), browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: configuration.runtime.Executable()}, func(ctx context.Context) error {
		if sensitive.HasValues(ctx) {
			if err := sensitive.Remember(ctx, key); err != nil {
				return errors.New("cannot protect model credentials in this browser session; use a new session")
			}
		}
		if err := checkpoints.Start(ctx, browsing.CheckpointLimits{MaxSteps: input.MaxSteps, TimeoutSeconds: input.TimeoutSeconds, MaxCompletionTokens: input.MaxCompletionTokens}, "navigate"); err != nil {
			return err
		}
		var err error
		findings, err = intelligence.Run(ctx, request, model, hooks)
		return err
	})
	result = action.Result{Session: &snapshot}
	if runErr != nil {
		if err := budget.Err(); err != nil {
			return result, err
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			return result, context.DeadlineExceeded
		}
		if errors.Is(runErr, context.Canceled) {
			return result, context.Canceled
		}
		return result, errors.New(redact(runErr.Error()))
	}
	output := BrowseOutput{Engine: browserruntime.Lightpanda, Result: redactBrowseResult(findings, redact)}
	encoded, err := json.Marshal(output)
	if err != nil || len(encoded) > 1024*1024 {
		return result, errors.New("browser findings exceed the result limit")
	}
	if err := validateBrowseJSON(a.validateOutput, encoded); err != nil {
		return result, errors.New("browser findings do not match the result schema")
	}
	evidence, err := json.Marshal(output.Evidence)
	if err != nil {
		return result, errors.New("cannot encode browser evidence")
	}
	result.Output = encoded
	result.Artifacts = []action.Artifact{{Name: "Browser findings and evidence", Files: []artifacts.FileSource{
		{Name: "findings.json", MediaType: "application/json", Data: encoded},
		{Name: "evidence.json", MediaType: "application/json", Data: evidence},
	}}}
	if err := budget.Err(); err != nil {
		return action.Result{Session: &snapshot}, err
	}
	return result, nil
}

func (input *BrowseInput) validate() error {
	if !utf8.ValidString(input.Objective) || strings.TrimSpace(input.Objective) == "" || utf8.RuneCountInString(input.Objective) > 4000 {
		return errors.New("objective must contain between 1 and 4000 nonblank characters")
	}
	if !id.Valid(input.SessionScope) || input.SessionID != "" && !id.Valid(input.SessionID) {
		return errors.New("browse requires canonical session_scope and session_id values")
	}
	if input.MaxSteps < 1 || input.MaxSteps > 30 || input.TimeoutSeconds < 5 || input.TimeoutSeconds > 180 || input.MaxCompletionTokens < 1024 || input.MaxCompletionTokens > 32768 {
		return errors.New("browse execution budgets are outside their supported limits")
	}
	target, err := url.Parse(input.URL)
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") || strings.TrimSpace(input.URL) != input.URL || len(input.URL) > 8192 || utf8.RuneCountInString(input.URL) > 4096 {
		return errors.New("url must be an absolute HTTP(S) URL without embedded credentials")
	}
	if _, err := url.QueryUnescape(target.RawQuery); err != nil {
		return errors.New("url query contains invalid percent encoding")
	}
	if len(input.AllowedHosts) == 0 {
		input.AllowedHosts = []string{"*"}
	}
	if len(input.AllowedHosts) == 1 && input.AllowedHosts[0] == "*" {
		return nil
	}
	if len(input.AllowedHosts) > 8 {
		return errors.New("allowed_hosts must contain at most eight exact hostnames")
	}
	seen := map[string]bool{}
	for i, host := range input.AllowedHosts {
		host = strings.ToLower(host)
		if !validBrowseHost(host) || seen[host] {
			return errors.New("allowed_hosts must be [\"*\"] or unique exact hostnames without ports, schemes or credentials")
		}
		seen[host], input.AllowedHosts[i] = true, host
	}
	if !seen[strings.ToLower(target.Hostname())] {
		return errors.New("url hostname must be included in allowed_hosts")
	}
	return nil
}

func checkBrowseURLCredential(raw, credential string) error {
	protected := errors.New("starting URL contains protected model credentials or excessive percent encoding")
	if strings.Contains(raw, credential) {
		return protected
	}
	target, err := url.Parse(raw)
	if err != nil {
		return errors.New("starting URL is invalid")
	}
	for _, component := range []struct {
		value string
		query bool
	}{{value: target.EscapedPath()}, {value: target.RawQuery, query: true}, {value: target.EscapedFragment()}} {
		value := component.value
		for round := 0; round < 8; round++ {
			if strings.Contains(value, credential) {
				return protected
			}
			decoded := unescapeBrowseURLComponent(value, component.query)
			if decoded == value {
				break
			}
			value = decoded
		}
		if strings.Contains(value, credential) || unescapeBrowseURLComponent(value, component.query) != value {
			return protected
		}
	}
	return nil
}

func unescapeBrowseURLComponent(value string, query bool) string {
	// A valid encoded percent can become literal text at the next layer. Keep
	// those literals while decoding other escapes; they must not hide a key.
	var escaped strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '%' && (i+2 >= len(value) || !browseHex(value[i+1]) || !browseHex(value[i+2])) {
			escaped.WriteString("%25")
		} else {
			escaped.WriteByte(value[i])
		}
	}
	var decoded string
	if query {
		decoded, _ = url.QueryUnescape(escaped.String())
	} else {
		decoded, _ = url.PathUnescape(escaped.String())
	}
	return decoded
}

func browseHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func validBrowseHost(host string) bool {
	if host == "" || len(host) > 253 || strings.TrimSpace(host) != host {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func validateBrowseJSON(validate func(any) error, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return validate(value)
}

func redactBrowseResult(value intelligence.Result, redact func(string) string) intelligence.Result {
	value.Status, value.Summary, value.FinalURL = redact(value.Status), redact(value.Summary), redact(value.FinalURL)
	value.Findings = append([]intelligence.Finding{}, value.Findings...)
	for i := range value.Findings {
		value.Findings[i].Text = redact(value.Findings[i].Text)
		value.Findings[i].EvidenceIDs = append([]string{}, value.Findings[i].EvidenceIDs...)
		for j := range value.Findings[i].EvidenceIDs {
			value.Findings[i].EvidenceIDs[j] = redact(value.Findings[i].EvidenceIDs[j])
		}
	}
	value.Evidence = append([]intelligence.Evidence{}, value.Evidence...)
	for i := range value.Evidence {
		item := &value.Evidence[i]
		item.ID, item.URL, item.Title, item.Text = redact(item.ID), redact(item.URL), redact(item.Title), redact(item.Text)
	}
	value.Steps = append([]intelligence.Step{}, value.Steps...)
	for i := range value.Steps {
		value.Steps[i].Action, value.Steps[i].Outcome = redact(value.Steps[i].Action), redact(value.Steps[i].Outcome)
	}
	return value
}

const browseInputSchema = `{
  "type":"object", "additionalProperties":false,
  "properties":{
    "objective":{"type":"string","minLength":1,"maxLength":4000},
    "url":{"type":"string","minLength":1,"maxLength":4096},
    "session_scope":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "session_id":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "allowed_hosts":{"type":"array","maxItems":8,"uniqueItems":true,"default":["*"],"description":"Omitted or empty allows all hostnames, equivalent to [\"*\"]. Otherwise list exact permitted hostnames, including the starting URL hostname. The wildcard must stand alone.","items":{"type":"string","minLength":1,"maxLength":253}},
    "max_steps":{"type":"integer","minimum":1,"maximum":30,"default":12},
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":180,"default":120},
    "max_completion_tokens":{"type":"integer","minimum":1024,"maximum":32768,"default":8192}
  }, "required":["objective","url","session_scope"]
}`

const browseOutputSchema = `{
  "type":"object", "additionalProperties":false,
  "properties":{
    "engine":{"type":"string","enum":["lightpanda"]},
    "status":{"type":"string","enum":["completed","budget_exhausted","incomplete"]}, "summary":{"type":"string"}, "final_url":{"type":"string"},
    "findings":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string"},"evidence_ids":{"type":"array","items":{"type":"string"}}},"required":["text","evidence_ids"]}},
    "evidence":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string"},"url":{"type":"string"},"title":{"type":"string"},"text":{"type":"string"}},"required":["id","url","title","text"]}},
    "steps":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"action":{"type":"string"},"outcome":{"type":"string"}},"required":["action","outcome"]}},
    "usage":{"type":"object","additionalProperties":false,"properties":{"steps":{"type":"integer","minimum":0},"model_calls":{"type":"integer","minimum":0},"completion_tokens_reserved":{"type":"integer","minimum":0}},"required":["steps","model_calls","completion_tokens_reserved"]}
  }, "required":["engine","status","summary","final_url","findings","evidence","steps","usage"]
}`
