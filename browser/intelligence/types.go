// Package intelligence runs bounded model-directed work on an existing CDP page.
package intelligence

import "context"

type Request struct {
	Objective           string
	URL                 string
	AllowedHosts        []string
	MaxSteps            int
	MaxCompletionTokens int
}

type Target struct {
	ID    int    `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

type Observation struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Text    string   `json:"text"`
	Targets []Target `json:"targets"`
}

// Command is the only model-to-browser protocol. Models never supply JavaScript,
// credentials, CDP commands or arbitrary selectors.
type Command struct {
	Action   string    `json:"action"`
	URL      string    `json:"url,omitempty"`
	Target   int       `json:"target,omitempty"`
	Text     string    `json:"text,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Question string    `json:"question,omitempty"`
	Options  []string  `json:"options,omitempty"`
}

type Finding struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type Evidence struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type Step struct {
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
}

// Clarification is retained only for this browsing execution's model decisions.
// It is not a browser result, an execution log or an evidence artifact.
type Clarification struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Answer   string   `json:"answer"`
}

type Turn struct {
	Objective      string          `json:"objective"`
	AllowedHosts   []string        `json:"allowed_hosts"`
	Observation    Observation     `json:"observation"`
	Evidence       []Evidence      `json:"evidence"`
	RecentSteps    []Step          `json:"recent_steps"`
	Clarifications []Clarification `json:"clarifications,omitempty"`
}

// Model implementations receive only safe observations. The token limit is a
// completion allowance, reserved before every call even if a provider omits usage.
type Model interface {
	Next(context.Context, Turn, int) (Command, error)
}

type Hooks struct {
	Log        func(context.Context, string) error
	Redact     func(string) string
	Question   func(context.Context, string, string, []string) (string, error)
	Checkpoint func(context.Context, Progress) error
}

type Usage struct {
	Steps                    int `json:"steps"`
	ModelCalls               int `json:"model_calls"`
	CompletionTokensReserved int `json:"completion_tokens_reserved"`
}

// EvidenceReference identifies already-redacted evidence without retaining its
// URL, title or text. SHA256 covers the canonical JSON encoding of Evidence.
type EvidenceReference struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// Progress records a pending command and metadata for preceding operations.
// The runtime bounds these fields to thirty steps, eight evidence references
// and four clarifications. It contains no browser or model content.
type Progress struct {
	Command        string              `json:"command,omitempty"`
	Usage          Usage               `json:"usage"`
	Steps          []Step              `json:"steps"`
	Evidence       []EvidenceReference `json:"evidence"`
	Clarifications int                 `json:"clarifications"`
}

type Result struct {
	Status   string     `json:"status"`
	Summary  string     `json:"summary"`
	FinalURL string     `json:"final_url"`
	Findings []Finding  `json:"findings"`
	Evidence []Evidence `json:"evidence"`
	Steps    []Step     `json:"steps"`
	Usage    Usage      `json:"usage"`
}
