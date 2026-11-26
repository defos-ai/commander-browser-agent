// Package contracts defines browser capabilities without a Commander or runtime dependency.
package contracts

import (
	"context"
	"strings"
	"unicode/utf8"
)

// BrowserAgent reasons about a bounded task on an already-owned session.
// Implementations must not create a replacement session or reuse expired access.
type BrowserAgent interface {
	Browse(context.Context, BrowseRequest, Hooks) (BrowseResult, error)
}

// ModelConfig is Go-only access to the protected model gateway. The caller owns
// APIKey and must clear it after the agent has joined its use. Implementations
// borrow that buffer without passing the real key to a reasoning runtime.
type ModelConfig struct {
	Provider string
	Model    string
	Endpoint string
	APIKey   []byte `json:"-"`
}

func (ModelConfig) String() string   { return "private browser model configuration" }
func (ModelConfig) GoString() string { return "private browser model configuration" }

// BrowserAgentFactory binds a configured agent to the current session callback.
// Construction starts no worker, browser connection or model request.
type BrowserAgentFactory func(context.Context, ModelConfig) (BrowserAgent, error)

// ModelValidator checks model access without browser work or reasoning. It
// borrows APIKey until return, honors cancellation and exposes no provider text.
type ModelValidator func(context.Context, ModelConfig) error

// BrowseRequest contains only a non-secret task and its execution allowances.
// Model credentials and provider configuration belong to the host adapter.
type BrowseRequest struct {
	Objective            string
	MaxIterations        int
	MaxTokensPerResponse int
	TimeoutSeconds       int
}

// BrowseResult contains protected runtime-reported text and request counts.
// It does not attest website effects, grounded evidence or actual token usage.
type BrowseResult struct {
	Status        string `json:"status"`
	Text          string `json:"text,omitempty"`
	ModelRequests int    `json:"model_requests,omitempty"`
}

// Event carries a bounded non-secret question or fixed progress/admission metadata.
// Browser code, tool arguments and raw runtime errors do not belong here.
type Event struct {
	Kind     string   `json:"kind"`
	Sequence int      `json:"sequence,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Text     string   `json:"text,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Valid checks a single hook event. The host checks ordering and execution limits.
func (e Event) Valid() bool {
	switch e.Kind {
	case "admit":
		return e.Tool == "browser_exec" && e.Sequence > 0 && e.Text == "" && len(e.Options) == 0
	case "progress":
		return (e.Tool == "" && e.Sequence == 0 || e.Tool == "browser_exec" && e.Sequence > 0) &&
			(e.Text == "started" || e.Text == "finished") && len(e.Options) == 0
	case "question":
		if e.Tool != "" || e.Sequence != 0 || !clarificationText(e.Text, 2000) || len(e.Options) > 4 {
			return false
		}
		for _, option := range e.Options {
			if !clarificationText(option, 500) {
				return false
			}
		}
		return true
	}
	return false
}

type Reply struct {
	Approved bool   `json:"approved,omitempty"`
	Answer   string `json:"answer,omitempty"`
}

// ValidFor requires approval for admission and an answer for clarification.
// Progress is a notification and its reply is ignored.
func (r Reply) ValidFor(event Event) bool {
	switch event.Kind {
	case "admit":
		return r.Approved && r.Answer == ""
	case "question":
		return clarificationText(r.Answer, 2000)
	case "progress":
		return true
	}
	return false
}

func clarificationText(text string, maxBytes int) bool {
	return len(text) <= maxBytes && utf8.ValidString(text) && !strings.ContainsRune(text, 0) && strings.TrimSpace(text) != ""
}

// Hooks must respect cancellation. Admission requires a positive approval;
// an absent hook or missing checkpoint receipt must never approve browser work.
// Answers are non-secret clarification only, not credential delivery.
type Hooks func(context.Context, Event) (Reply, error)
