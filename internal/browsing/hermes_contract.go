package browsing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/defos-ai/commander/framework/agent/action"
)

// HermesBrowseInput describes native reasoning allowances. Wall time includes
// clarification waits after model setup has completed its separate bound.
type HermesBrowseInput struct {
	Objective            string   `json:"objective"`
	URL                  string   `json:"url"`
	AllowedHosts         []string `json:"allowed_hosts,omitempty"`
	MaxIterations        int      `json:"max_iterations,omitempty"`
	MaxTokensPerResponse int      `json:"max_tokens_per_response,omitempty"`
	TimeoutSeconds       int      `json:"timeout_seconds,omitempty"`
}

func DecodeHermesBrowseInput(raw json.RawMessage) (HermesBrowseInput, error) {
	input := HermesBrowseInput{MaxIterations: 12, MaxTokensPerResponse: 2048, TimeoutSeconds: 120}
	if len(raw) > 128*1024 || !utf8.Valid(raw) || !hermesBrowseFields(raw) || action.DecodeInput(raw, &input) != nil {
		return HermesBrowseInput{}, errors.New("native browse input does not match its schema")
	}
	if !utf8.ValidString(input.Objective) || strings.TrimSpace(input.Objective) == "" || strings.ContainsRune(input.Objective, 0) || utf8.RuneCountInString(input.Objective) > 4000 {
		return HermesBrowseInput{}, errors.New("objective must contain between 1 and 4000 nonblank UTF-8 characters")
	}
	if input.MaxIterations < 1 || input.MaxIterations > 30 || input.MaxTokensPerResponse < 1 || input.MaxTokensPerResponse > 16384 || input.TimeoutSeconds < 5 || input.TimeoutSeconds > 180 {
		return HermesBrowseInput{}, errors.New("native browse execution allowances are outside their supported limits")
	}
	target, err := url.Parse(input.URL)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Scheme != "http" && target.Scheme != "https" ||
		strings.TrimSpace(input.URL) != input.URL || len(input.URL) > 8192 || utf8.RuneCountInString(input.URL) > 4096 {
		return HermesBrowseInput{}, errors.New("url must be a bounded absolute HTTP(S) URL without embedded credentials")
	}
	if _, err := url.QueryUnescape(target.RawQuery); err != nil {
		return HermesBrowseInput{}, errors.New("url query contains invalid percent encoding")
	}
	if len(input.ObjectiveForHermes()) > 16384 {
		return HermesBrowseInput{}, errors.New("combined starting URL and objective exceed the native task size limit")
	}
	if len(input.AllowedHosts) == 0 {
		input.AllowedHosts = []string{"*"}
	}
	if len(input.AllowedHosts) != 1 || input.AllowedHosts[0] != "*" {
		return HermesBrowseInput{}, errors.New("native browsing supports only allowed_hosts [\"*\"]; exact host restrictions are unavailable")
	}
	return input, nil
}

// ObjectiveForHermes combines a decoded starting URL and objective as one
// bounded task. It does not imply an independent host navigation occurred.
func (input HermesBrowseInput) ObjectiveForHermes() string {
	return "Starting URL:\n" + input.URL + "\n\nObjective:\n" + input.Objective
}

// The typed decoder matches keys without case sensitivity and accepts duplicate
// fields. Refuse both before it can collapse a conflicting scope or allowance.
func hermesBrowseFields(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool, 6)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		switch name {
		case "objective", "url", "allowed_hosts", "max_iterations", "max_tokens_per_response", "timeout_seconds":
		default:
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}

// HermesBrowseOutput contains protected model-reported text and runtime counts.
// Completion establishes no independent evidence grounding or token/cost usage.
type HermesBrowseOutput struct {
	Runtime       string `json:"runtime"`
	Engine        string `json:"engine"`
	Status        string `json:"status"`
	Text          string `json:"text"`
	ModelRequests int    `json:"model_requests"`
}

// Validate enforces UTF-8 byte bounds, which JSON Schema's character length
// cannot express. Call it after redaction and before finalization or artifacts.
func (output HermesBrowseOutput) Validate() error {
	validStatus := false
	switch output.Status {
	case "completed", "incomplete", "interrupted", "failed", "uncertain":
		validStatus = true
	}
	if output.Runtime != "hermes" || output.Engine != "lightpanda" || !validStatus || !utf8.ValidString(output.Text) || len(output.Text) > 65536 || output.ModelRequests < 0 || output.ModelRequests > 1_000_000 {
		return errors.New("native browse result exceeds its supported metadata or text bounds")
	}
	return nil
}

const HermesBrowseInputSchema = `{
  "type":"object","additionalProperties":false,
  "description":"Native Hermes browsing with model-reported text results. Starting URL and objective form a task of at most 16384 UTF-8 bytes. Execution wall time includes clarification after separately bounded model setup.",
  "properties":{
    "objective":{"type":"string","minLength":1,"maxLength":4000,"description":"Nonblank browser objective; up to 4000 UTF-8 characters."},
    "url":{"type":"string","minLength":1,"maxLength":4096,"description":"Starting HTTP(S) URL without embedded credentials; at most 8192 UTF-8 bytes."},
    "allowed_hosts":{"type":"array","maxItems":1,"default":["*"],"items":{"type":"string","enum":["*"]},"description":"Omitted or empty means [\"*\"] and permits all HTTP(S) hostnames. Exact host restrictions are unsupported and rejected."},
    "max_iterations":{"type":"integer","minimum":1,"maximum":30,"default":12,"description":"Native reasoning iteration allowance; a final wrap-up model request may also occur."},
    "max_tokens_per_response":{"type":"integer","minimum":1,"maximum":16384,"default":2048,"description":"Completion allowance per model response. Actual token usage and cost are unreported."},
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":180,"default":120,"description":"Wall-clock execution allowance including startup, checkpoint fences, model requests, clarification, native work and final persistence, after separately bounded model setup."}
  },"required":["objective","url"]
}`

const HermesBrowseOutputSchema = `{
  "type":"object","additionalProperties":false,
  "description":"Protected native Hermes result. Text is model-reported, with no independently grounded evidence, actual token usage or cost claim.",
  "properties":{
    "runtime":{"type":"string","enum":["hermes"]},
    "engine":{"type":"string","enum":["lightpanda"]},
    "status":{"type":"string","enum":["completed","incomplete","interrupted","failed","uncertain"],"description":"Runtime outcome; completed does not establish independent verification of findings or external effects."},
    "text":{"type":"string","maxLength":65536,"description":"Protected model-reported final text, at most 65536 UTF-8 bytes; empty text is encoded explicitly."},
    "model_requests":{"type":"integer","minimum":0,"maximum":1000000,"description":"Hermes-reported api_calls counter; it can omit internal retries. Provider forwarding has a separate bound; actual token usage and cost are unreported."}
  },"required":["runtime","engine","status","text","model_requests"]
}`
