// Package hermes hosts the pinned external reasoning runtime. It does not
// implement a model loop or expose a Commander action catalog.
package hermes

import (
	"context"

	"github.com/defos-ai/commander/framework/domain/sessions"
)

// Config binds one private worker to a framework-issued creation claim.
// Paths come from trusted runtime discovery, never model arguments.
type Config struct {
	Python     string
	Source     string
	Lightpanda string
	Claim      sessions.CreationClaim
}

// Request describes one execution. Endpoint is the exact selected completion
// endpoint. The real APIKey remains in Go; the caller owns and clears its buffer
// after Run returns. Hermes receives only an ephemeral local transport credential.
type Request struct {
	Objective      string `json:"objective"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
	APIKey         []byte `json:"-"`
	MaxIterations  int    `json:"max_iterations"`
	MaxTokens      int    `json:"max_tokens"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (Request) String() string   { return "private Hermes execution request" }
func (Request) GoString() string { return "private Hermes execution request" }

// workerRequest is the only model setup sent over the private worker pipe.
// Neither the real endpoint nor its managed key belongs to this payload.
type workerRequest struct {
	Objective      string `json:"objective"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	BaseURL        string `json:"base_url"`
	APIKey         []byte `json:"api_key"`
	MaxIterations  int    `json:"max_iterations"`
	MaxTokens      int    `json:"max_tokens"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ModelRelay     bool   `json:"model_relay,omitempty"`
}

// Event contains only a bounded question or fixed progress/admission metadata.
// Browser code, tool arguments and raw runtime errors never cross this boundary.
type Event struct {
	Kind     string   `json:"kind"`
	Sequence int      `json:"sequence,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Text     string   `json:"text,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type Reply struct {
	Approved bool   `json:"approved,omitempty"`
	Answer   string `json:"answer,omitempty"`
}

// Handler must respect cancellation. Admission is refused unless the handler
// positively confirms it; a missing checkpoint receipt is never approval.
type Handler func(context.Context, Event) (Reply, error)

// Result deliberately excludes transcripts, reasoning and provider errors.
// Tool admission fences a whole Python invocation, including its uncertain effects.
type Result struct {
	Status        string `json:"status"`
	Text          string `json:"text,omitempty"`
	ModelRequests int    `json:"model_requests,omitempty"`
}
