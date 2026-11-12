package browser_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander/framework/agent/action"
)

func TestHermesBrowseContractDefaultsAndWildcardScope(t *testing.T) {
	for _, hosts := range []string{"", `,"allowed_hosts":[]`, `,"allowed_hosts":["*"]`} {
		raw := json.RawMessage(`{"objective":"Read the 漢字 study","url":"https://example.test/study"` + hosts + `}`)
		input, err := browsing.DecodeHermesBrowseInput(raw)
		if err != nil || input.Objective != "Read the 漢字 study" || input.MaxIterations != 12 || input.MaxTokensPerResponse != 2048 || input.TimeoutSeconds != 120 || !reflect.DeepEqual(input.AllowedHosts, []string{"*"}) {
			t.Fatal("native browse defaults changed or empty host scope restricted browsing")
		}
		input.AllowedHosts[0] = "caller mutation"
		next, err := browsing.DecodeHermesBrowseInput(raw)
		if err != nil || !reflect.DeepEqual(next.AllowedHosts, []string{"*"}) {
			t.Fatal("native browse defaults share mutable host scope")
		}
	}
	maximum := json.RawMessage(`{"objective":"Read the study","url":"http://127.0.0.1:4275/","max_iterations":30,"max_tokens_per_response":16384,"timeout_seconds":180}`)
	if input, err := browsing.DecodeHermesBrowseInput(maximum); err != nil || input.MaxIterations != 30 || input.MaxTokensPerResponse != 16384 || input.TimeoutSeconds != 180 {
		t.Fatal("native browse rejected its explicit maximum allowances")
	}
}

func TestHermesBrowseContractRejectsLegacyScopeAndUnsafeInput(t *testing.T) {
	base := map[string]any{"objective": "Read the study", "url": "https://example.test/study"}
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"exact_host", "allowed_hosts", []string{"example.test"}},
		{"mixed_wildcard", "allowed_hosts", []string{"*", "example.test"}},
		{"legacy_steps", "max_steps", 12},
		{"legacy_total_tokens", "max_completion_tokens", 8192},
		{"unknown_field", "unknown_fixture_field", true},
		{"blank_objective", "objective", " \n\t "},
		{"nul_objective", "objective", "Read\x00the study"},
		{"long_objective", "objective", strings.Repeat("漢", 4001)},
		{"relative_url", "url", "/study"},
		{"embedded_credentials", "url", "https://private-user:private-password@example.test/"},
		{"unsupported_scheme", "url", "file:///tmp/study"},
		{"malformed_query", "url", "https://example.test/?q=%zz"},
		{"url_character_bound", "url", "https://example.test/" + strings.Repeat("x", 4096)},
		{"url_byte_bound", "url", "https://example.test/" + strings.Repeat("漢", 3000)},
		{"zero_iterations", "max_iterations", 0},
		{"excess_iterations", "max_iterations", 31},
		{"zero_response_allowance", "max_tokens_per_response", 0},
		{"excess_response_allowance", "max_tokens_per_response", 16385},
		{"short_wall_time", "timeout_seconds", 4},
		{"excess_wall_time", "timeout_seconds", 181},
		{"null_scope", "allowed_hosts", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]any{}
			for name, value := range base {
				input[name] = value
			}
			input[test.field] = test.value
			raw, _ := json.Marshal(input)
			decoded, err := browsing.DecodeHermesBrowseInput(raw)
			if err == nil || !reflect.DeepEqual(decoded, browsing.HermesBrowseInput{}) || strings.Contains(err.Error(), "private-user") || strings.Contains(err.Error(), "private-password") {
				t.Fatal("native browse accepted unsupported scope, legacy input or unsafe data")
			}
		})
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`{"objective":"Read","url":"https://example.test/"} {}`),
		json.RawMessage(`{"Objective":"Read","url":"https://example.test/"}`),
		json.RawMessage(`{"objective":"Read","url":"https://example.test/","MAX_ITERATIONS":30}`),
		json.RawMessage(`{"objective":"Read","url":"https://example.test/","allowed_hosts":["private.test"],"allowed_hosts":["*"]}`),
		json.RawMessage(`{"objective":"Read","url":"https://example.test/","max_iterations":30,"max_iterations":1}`),
		append([]byte(`{"objective":"`), append([]byte{0xff}, []byte(`","url":"https://example.test/"}`)...)...),
	} {
		if _, err := browsing.DecodeHermesBrowseInput(raw); err == nil {
			t.Fatal("native browse accepted malformed or invalid UTF-8 JSON")
		}
	}
}

func TestHermesBrowseContractBoundsCombinedNativeTask(t *testing.T) {
	input := browsing.HermesBrowseInput{Objective: strings.Repeat("😀", 4000), URL: "https://example.test/"}
	input.URL += strings.Repeat("x", 16384-len(input.ObjectiveForHermes()))
	// The URL and objective each fit their own character and byte limits.
	// Their native task wrapper must fit as well, before setup can ask questions.
	for _, extra := range []string{"", "x"} {
		candidate := input
		candidate.URL += extra
		raw, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal("native task boundary fixture could not be encoded")
		}
		decoded, err := browsing.DecodeHermesBrowseInput(raw)
		if extra == "" {
			if err != nil || len(decoded.ObjectiveForHermes()) != 16384 || decoded.ObjectiveForHermes() != "Starting URL:\n"+input.URL+"\n\nObjective:\n"+input.Objective {
				t.Fatal("native browse rejected or changed its bounded task wrapper")
			}
		} else if err == nil {
			t.Fatal("native browse accepted a task beyond the worker byte limit")
		}
	}
}

func TestHermesBrowseResultHasHonestSchemaAndByteBounds(t *testing.T) {
	inputSchema, err := action.ResolveSchema(json.RawMessage(browsing.HermesBrowseInputSchema))
	if err != nil {
		t.Fatal("native browse input schema is invalid")
	}
	outputSchema, err := action.ResolveSchema(json.RawMessage(browsing.HermesBrowseOutputSchema))
	if err != nil {
		t.Fatal("native browse output schema is invalid")
	}
	input := map[string]any{"objective": "Read the study", "url": "https://example.test/", "allowed_hosts": []any{}}
	if inputSchema.Validate(input) != nil {
		t.Fatal("native browse schema rejected its default host semantics")
	}
	input["allowed_hosts"] = []any{"example.test"}
	if inputSchema.Validate(input) == nil {
		t.Fatal("native browse schema advertised unsupported exact host enforcement")
	}
	output := browsing.HermesBrowseOutput{Runtime: "hermes", Engine: "lightpanda", Status: "completed"}
	encoded, err := json.Marshal(output)
	var value map[string]any
	if err != nil || json.Unmarshal(encoded, &value) != nil || output.Validate() != nil || outputSchema.Validate(value) != nil || len(value) != 5 || !bytes.Contains(encoded, []byte(`"text":""`)) || !bytes.Contains(encoded, []byte(`"model_requests":0`)) {
		t.Fatal("native browse result omitted explicit text/counts or expanded its metadata contract")
	}
	value["evidence"] = []any{}
	if outputSchema.Validate(value) == nil {
		t.Fatal("native browse schema accepted invented evidence metadata")
	}
	for _, status := range []string{"completed", "incomplete", "interrupted", "failed", "uncertain"} {
		output.Status, output.Text, output.ModelRequests = status, strings.Repeat("x", 65536), 1_000_000
		if output.Validate() != nil {
			t.Fatal("native browse rejected its bounded text/status result")
		}
	}
	for _, test := range []browsing.HermesBrowseOutput{
		{Runtime: "hermes", Engine: "lightpanda", Status: "budget_exhausted"},
		{Runtime: "hermes", Engine: "lightpanda", Status: "completed", Text: strings.Repeat("漢", 21846)},
		{Runtime: "hermes", Engine: "lightpanda", Status: "completed", Text: string([]byte{0xff})},
		{Runtime: "hermes", Engine: "lightpanda", Status: "completed", ModelRequests: 1_000_001},
		{Runtime: "hermes", Engine: "lightpanda", Status: "completed", ModelRequests: -1},
		{Runtime: "other", Engine: "lightpanda", Status: "completed"},
	} {
		if test.Validate() == nil {
			t.Fatal("native browse result accepted invalid metadata or exceeded its UTF-8 byte limit")
		}
	}
}
