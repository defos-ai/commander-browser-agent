package intelligence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
)

// Run drives the retained page through typed operations. A browser operation is
// never retried automatically: failure may mean its side effect is uncertain.
func Run(ctx context.Context, request Request, model Model, hooks Hooks) (result Result, runErr error) {
	result = Result{Status: "incomplete", Findings: []Finding{}, Evidence: []Evidence{}, Steps: []Step{}}
	if model == nil || request.MaxSteps < 1 || request.MaxSteps > 30 ||
		request.MaxCompletionTokens < 1024 || request.MaxCompletionTokens > 32768 ||
		len(request.AllowedHosts) > 8 || strings.TrimSpace(request.Objective) == "" ||
		!utf8.ValidString(request.Objective) || utf8.RuneCountInString(request.Objective) > 4000 {
		return result, errors.New("invalid browse runtime configuration or budgets")
	}
	callerRedact := hooks.Redact
	hooks.Redact = func(text string) string {
		if callerRedact != nil {
			text = callerRedact(text)
		}
		return sensitive.Redact(ctx, text)
	}
	log := func(message string) error {
		if hooks.Log == nil {
			return nil
		}
		return hooks.Log(ctx, message)
	}
	if err := log("Opening the Lightpanda page"); err != nil {
		return result, err
	}
	page, err := newPage(ctx, request.URL, request.AllowedHosts)
	if err != nil {
		return result, err
	}
	request.AllowedHosts = make([]string, 0, len(page.hosts))
	for host := range page.hosts {
		request.AllowedHosts = append(request.AllowedHosts, host)
	}
	slices.Sort(request.AllowedHosts)
	modelHosts := slices.Clone(request.AllowedHosts)
	for i, host := range modelHosts {
		modelHosts[i] = hooks.Redact(host)
	}
	defer func() {
		// The last command may have navigated just before a budget stopped the
		// loop. Report the current confirmed URL, rather than the previous turn.
		final, cancel := context.WithTimeout(ctx, time.Second)
		address, err := page.frameURL(final)
		cancel()
		result.FinalURL = ""
		if err == nil {
			result.FinalURL = protectedURL(address, hooks.Redact)
		}
		if err := page.Close(); err != nil {
			runErr = errors.Join(runErr, errors.New("cannot finish browser navigation cleanup; inspect the session"))
		}
	}()
	if err := page.Navigate(ctx, request.URL); err != nil {
		return result, errors.New("cannot open the starting page within the permitted browser scope")
	}
	remaining := request.MaxCompletionTokens
	var clarifications []Clarification
	for step := 0; step < request.MaxSteps && remaining > 0; step++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		observation, err := page.Observe(ctx)
		if err != nil {
			return result, errors.New("cannot observe the retained browser page")
		}
		observation = safeObservation(observation, hooks.Redact)
		result.FinalURL = observation.URL
		recent := result.Steps
		if len(recent) > 6 {
			recent = recent[len(recent)-6:]
		}
		// Reserve the full allowance before sending, independently of provider
		// usage reporting. Input bytes and response bytes are separately bounded.
		allowance := min(remaining, 1024)
		remaining -= allowance
		result.Usage.ModelCalls++
		result.Usage.CompletionTokensReserved += allowance
		if err := log(fmt.Sprintf("Requesting browser decision %d", step+1)); err != nil {
			return result, err
		}
		turn := Turn{Objective: hooks.Redact(request.Objective), AllowedHosts: modelHosts, Observation: observation,
			Evidence: modelEvidence(result.Evidence), RecentSteps: recent, Clarifications: modelClarifications(clarifications)}
		command, err := model.Next(ctx, turn, allowance)
		if err != nil {
			return result, errors.New("browser model request failed or returned an invalid command")
		}
		if err := validCommand(command, result.Evidence); err != nil {
			return result, err
		}
		command = safeCommand(command, hooks.Redact)
		if err := validCommand(command, result.Evidence); err != nil {
			return result, err
		}
		if command.Action == "extract" && len(result.Evidence) >= 8 {
			result.Summary, result.Status = "The evidence limit was reached.", "budget_exhausted"
			if err := log("Browser evidence budget reached"); err != nil {
				return result, err
			}
			return result, nil
		}
		if command.Action == "ask" && len(clarifications) >= 4 {
			result.Summary, result.Status = "The browser clarification limit was reached.", "budget_exhausted"
			if err := log("Browser clarification limit reached"); err != nil {
				return result, err
			}
			return result, nil
		}
		result.Usage.Steps++
		if err := log("Running browser command: " + command.Action); err != nil {
			return result, err
		}
		if hooks.Checkpoint != nil {
			progress := ProgressSnapshot(result)
			progress.Command, progress.Clarifications = command.Action, len(clarifications)
			err := hooks.Checkpoint(ctx, progress)
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return result, context.DeadlineExceeded
				}
				if errors.Is(err, context.Canceled) {
					return result, context.Canceled
				}
				return result, errors.New("cannot persist browser progress; inspect session before retrying")
			}
		}
		var operationErr error
		switch command.Action {
		case "navigate":
			operationErr = page.Navigate(ctx, command.URL)
		case "click":
			operationErr = page.Click(ctx, command.Target)
		case "type_text":
			operationErr = page.Type(ctx, command.Target, command.Text)
		case "extract":
			var evidence Evidence
			evidence, operationErr = page.Extract(ctx, command.Target)
			if operationErr == nil {
				evidence.ID = fmt.Sprintf("e%d", len(result.Evidence)+1)
				evidence.URL = protectedURL(evidence.URL, hooks.Redact)
				evidence.Title = bounded(hooks.Redact(evidence.Title), 300)
				evidence.Text = bounded(hooks.Redact(evidence.Text), 4000)
				if evidence.Text == "" {
					operationErr = errors.New("the selected page evidence is empty")
				} else {
					result.Evidence = append(result.Evidence, evidence)
				}
			}
		case "wait":
			select {
			case <-ctx.Done():
				operationErr = ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		case "ask":
			if hooks.Question == nil {
				return result, errors.New("browser clarification requires the execution question bridge")
			}
			if err := log("Waiting for a non-secret browser clarification"); err != nil {
				return result, err
			}
			key := fmt.Sprintf("browser_clarification_%d", len(clarifications)+1)
			answer, err := hooks.Question(ctx, key, command.Question, slices.Clone(command.Options))
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return result, fmt.Errorf("browser clarification timed out: %w", context.DeadlineExceeded)
				}
				if errors.Is(err, context.Canceled) {
					return result, context.Canceled
				}
				return result, errors.New("browser clarification could not be answered")
			}
			if !validClarificationText(answer, 2000) {
				return result, errors.New("browser clarification answer must be nonblank UTF-8 of at most 2000 bytes")
			}
			answer = strings.TrimSpace(hooks.Redact(answer))
			if !validClarificationText(answer, 2000) {
				return result, errors.New("browser clarification answer is invalid after redaction")
			}
			clarifications = append(clarifications, Clarification{Question: command.Question, Options: slices.Clone(command.Options), Answer: answer})
			if err := log("Browser clarification answered; continuing"); err != nil {
				return result, err
			}
		case "finish":
			result.Status, result.Summary, result.Findings = "completed", command.Summary, command.Findings
			if result.Findings == nil {
				result.Findings = []Finding{}
			}
		}
		outcome := "completed"
		if operationErr != nil {
			outcome = "unconfirmed"
		}
		result.Steps = append(result.Steps, Step{Action: command.Action, Outcome: outcome})
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if operationErr != nil {
			result.Summary = "A browser command could not be confirmed. Inspect the session before retrying."
			if err := log("Browser command could not be confirmed; stopping"); err != nil {
				return result, err
			}
			return result, nil
		}
		if result.Status == "completed" {
			if err := log("Browser objective completed"); err != nil {
				return result, err
			}
			return result, nil
		}
	}
	result.Status, result.Summary = "budget_exhausted", "The browser step or completion-token allowance was reached."
	if err := log("Browser execution budget reached"); err != nil {
		return result, err
	}
	return result, nil
}

// ProgressSnapshot derives content-free metadata from a bounded, already-redacted
// runtime result. It returns independently owned slices with no pending command.
// Completed ask steps supply the final clarification count; the active loop
// supplies its current count before each pending command separately.
func ProgressSnapshot(result Result) Progress {
	progress := Progress{Usage: result.Usage, Steps: slices.Clone(result.Steps), Evidence: make([]EvidenceReference, len(result.Evidence))}
	for _, step := range result.Steps {
		if step.Action == "ask" && step.Outcome == "completed" {
			progress.Clarifications++
		}
	}
	for i, evidence := range result.Evidence {
		// Evidence contains strings only, so JSON encoding cannot fail.
		encoded, _ := json.Marshal(evidence)
		digest := sha256.Sum256(encoded)
		clear(encoded)
		progress.Evidence[i] = EvidenceReference{ID: evidence.ID, SHA256: fmt.Sprintf("%x", digest)}
	}
	return progress
}

func validCommand(command Command, evidence []Evidence) error {
	invalid := errors.New("model returned an invalid browser command")
	if len(command.URL) > 4096 || len(command.Text) > 2000 || len(command.Summary) > 2000 || command.Target < 0 || command.Target > 1000000 {
		return invalid
	}
	if command.Action != "finish" && (command.Summary != "" || len(command.Findings) != 0) {
		return invalid
	}
	if command.Action != "navigate" && command.URL != "" || command.Action != "type_text" && command.Text != "" {
		return invalid
	}
	if command.Action != "ask" && (command.Question != "" || len(command.Options) != 0) {
		return invalid
	}
	switch command.Action {
	case "navigate":
		if command.URL == "" || command.Target != 0 {
			return invalid
		}
	case "click":
		if command.Target == 0 {
			return invalid
		}
	case "type_text":
		if command.Target == 0 || strings.TrimSpace(command.Text) == "" {
			return invalid
		}
	case "extract":
	case "wait":
		if command.Target != 0 {
			return invalid
		}
	case "ask":
		if command.Target != 0 || !validClarificationText(command.Question, 1000) || len(command.Options) > 4 {
			return invalid
		}
		seen := make(map[string]bool, len(command.Options))
		for _, option := range command.Options {
			if !validClarificationText(option, 160) || seen[strings.ToLower(option)] {
				return invalid
			}
			seen[strings.ToLower(option)] = true
		}
	case "finish":
		if command.Target != 0 || strings.TrimSpace(command.Summary) == "" || len(command.Findings) > 12 {
			return invalid
		}
		if len(evidence) == 0 || len(command.Findings) == 0 {
			return errors.New("model completion requires extracted evidence and cited findings")
		}
		known := map[string]bool{}
		for _, item := range evidence {
			known[item.ID] = true
		}
		for _, finding := range command.Findings {
			if strings.TrimSpace(finding.Text) == "" || len(finding.Text) > 1000 || len(finding.EvidenceIDs) == 0 || len(finding.EvidenceIDs) > 8 {
				return invalid
			}
			for _, reference := range finding.EvidenceIDs {
				if !known[reference] {
					return errors.New("model finding references evidence that was not extracted")
				}
			}
		}
	default:
		return invalid
	}
	return nil
}

func safeObservation(value Observation, redact func(string) string) Observation {
	value.URL = protectedURL(value.URL, redact)
	value.Title = bounded(redact(value.Title), 300)
	value.Text = bounded(redact(value.Text), 10000)
	for i := range value.Targets {
		value.Targets[i].Label = bounded(redact(value.Targets[i].Label), 160)
		value.Targets[i].URL = protectedURL(value.Targets[i].URL, redact)
	}
	return value
}

func safeCommand(value Command, redact func(string) string) Command {
	value.URL, value.Text, value.Summary = redact(value.URL), redact(value.Text), redact(value.Summary)
	value.Question = strings.TrimSpace(redact(value.Question))
	value.Options = slices.Clone(value.Options)
	for i, option := range value.Options {
		value.Options[i] = strings.TrimSpace(redact(option))
	}
	for i := range value.Findings {
		value.Findings[i].Text = redact(value.Findings[i].Text)
		for j := range value.Findings[i].EvidenceIDs {
			value.Findings[i].EvidenceIDs[j] = redact(value.Findings[i].EvidenceIDs[j])
		}
	}
	return value
}

func validClarificationText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) != ""
}

// Redact decoded URL components before serializing. RawPath may otherwise retain
// a percent-encoded credential even when visible page text was safely redacted.
func protectedURL(value string, redact func(string) string) string {
	address, err := pageURL(value)
	if err != nil {
		return ""
	}
	address.Path, address.Host = redact(address.Path), redact(address.Host)
	address.RawPath, address.RawQuery, address.Fragment, address.RawFragment = "", "", "", ""
	address.ForceQuery = false
	return bounded(address.String(), 2048)
}

func modelEvidence(evidence []Evidence) []Evidence {
	items := make([]Evidence, len(evidence))
	copy(items, evidence)
	for i := range items {
		items[i].Text = bounded(items[i].Text, 2000)
	}
	return items
}

func modelClarifications(clarifications []Clarification) []Clarification {
	items := slices.Clone(clarifications)
	for i := range items {
		items[i].Options = slices.Clone(items[i].Options)
	}
	return items
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}
