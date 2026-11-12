// Package login performs deterministic password and one-time-code login on a retained page.
package login

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
)

type Request struct {
	URL             string
	VerificationURL string
	ExpectedText    string
	// OneTimeCode is called once, only after a supported code form is inspected.
	// The executor owns and clears its returned bytes, including on errors.
	OneTimeCode func(context.Context) ([]byte, error)
}

type Credentials struct {
	Username []byte
	Password []byte
}

type Result struct {
	Status    string `json:"status"`
	Submitted bool   `json:"submitted"`
	Reason    string `json:"reason"`
}

var errBrowser = errors.New("login browser operation could not be completed safely")

// Run owns and clears returned credential/code bytes. It submits at most one
// password form and one code form, without retrying rejected or uncertain effects.
func Run(ctx context.Context, request Request, credentials func(context.Context) (Credentials, error)) (result Result, runErr error) {
	return run(ctx, request, credentials, newDriver)
}

func validateRequest(ctx context.Context, request Request, credentials func(context.Context) (Credentials, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, context.Cause(ctx))
	}
	origin, err := browsing.LoginOrigin(request.URL)
	verificationOrigin, verificationErr := browsing.LoginOrigin(request.VerificationURL)
	if err != nil || verificationErr != nil || origin != verificationOrigin || credentials == nil ||
		!utf8.ValidString(request.ExpectedText) || strings.TrimSpace(request.ExpectedText) == "" || len(request.ExpectedText) > 256 || strings.ContainsRune(request.ExpectedText, '\x00') {
		return "", errors.New("login requires same-origin URLs and a bounded non-secret verification marker")
	}
	return origin, nil
}

func run(ctx context.Context, request Request, credentials func(context.Context) (Credentials, error), open func(context.Context, string) (*driver, error)) (result Result, runErr error) {
	result = Result{Status: "unverified", Reason: "not_attempted"}
	origin, err := validateRequest(ctx, request, credentials)
	if err != nil {
		return result, err
	}
	page, err := open(ctx, origin)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errBrowser
	}
	defer func() {
		if err := page.close(); err != nil {
			runErr = errors.Join(runErr, errBrowser)
		}
		if ctx.Err() != nil {
			runErr = errors.Join(runErr, ctx.Err(), context.Cause(ctx))
		}
	}()
	// An explicit continuation may already be on a live challenge with private
	// server state. Inspect it before any verification navigation can discard it.
	if form, err := page.inspect(ctx); err == nil && isChallenge(form) {
		return resolveChallenge(ctx, page, request, form, result)
	}
	if err := page.navigate(ctx, request.VerificationURL); err != nil {
		result.Reason = "verification_unavailable"
		return result, nil
	}
	if form, err := page.inspect(ctx); err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	} else if isChallenge(form) {
		return resolveChallenge(ctx, page, request, form, result)
	}
	if verified, err := page.verify(ctx, request.VerificationURL, request.ExpectedText); err != nil {
		return result, errBrowser
	} else if verified {
		return Result{Status: "verified", Reason: "existing_session"}, nil
	}
	if err := page.navigate(ctx, request.URL); err != nil {
		result.Status, result.Reason = "unsupported_form", "login_navigation_unavailable"
		return result, nil
	}
	form, err := page.inspect(ctx)
	if err != nil {
		return result, errBrowser
	}
	if isChallenge(form) {
		return resolveChallenge(ctx, page, request, form, result)
	}
	if form.Status != "supported" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
	}
	form, err = submitPassword(ctx, page, credentials)
	if err != nil {
		return result, err
	}
	result.Status, result.Submitted, result.Reason = "submission_uncertain", form.Submitted, "submission_unconfirmed"
	if form.Status != "submitted" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
	}
	if err := page.settle(ctx); err != nil {
		return result, nil
	}
	if !page.hasSubmissionResponse() {
		return result, nil
	}
	page.clearFields(ctx)
	return verifySubmitted(ctx, page, request, result, false)
}

func submitPassword(ctx context.Context, page *driver, credentials func(context.Context) (Credentials, error)) (formState, error) {
	values, err := credentials(ctx)
	defer clear(values.Username)
	defer clear(values.Password)
	if err != nil {
		if ctx.Err() != nil {
			return formState{}, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return formState{}, err
		}
		return formState{}, errors.New("managed login credentials are unavailable")
	}
	for _, value := range [][]byte{values.Username, values.Password} {
		if !validValue(value, 1024) {
			return formState{}, errors.New("managed login credentials are invalid")
		}
	}
	if err := sensitive.Remember(ctx, values.Username, values.Password); err != nil {
		return formState{}, errors.New("login credential protection is unavailable")
	}
	form, err := page.submit(ctx, values)
	if err != nil {
		return formState{Status: "submission_uncertain", Reason: "submission_unconfirmed", Submitted: true}, nil
	}
	return form, nil
}

func isChallenge(form formState) bool {
	return form.Status == "requires_challenge" || form.Status == "supported_challenge"
}

func resolveChallenge(ctx context.Context, page *driver, request Request, form formState, result Result) (Result, error) {
	result.Status, result.Reason = "requires_challenge", form.Reason
	if form.Status != "supported_challenge" || request.OneTimeCode == nil {
		return result, nil
	}
	// Request only once. An expired value or a rejected code requires a new,
	// explicit execution with fresh observations, never an automatic new request.
	code, err := request.OneTimeCode(ctx)
	defer clear(code)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		result.Reason = "code_unavailable"
		return result, nil
	}
	if !validValue(code, 128) {
		result.Reason = "invalid_code"
		return result, nil
	}
	if err := sensitive.Remember(ctx, code); err != nil {
		return result, errors.New("login code protection is unavailable")
	}
	form, err = page.submitChallenge(ctx, code)
	clear(code)
	if err != nil {
		result.Status, result.Submitted, result.Reason = "submission_uncertain", true, "code_submission_unconfirmed"
		return result, nil
	}
	result.Submitted = result.Submitted || form.Submitted
	if form.Status != "submitted" {
		result.Reason = form.Reason
		if form.Status == "submission_uncertain" {
			result.Status = form.Status
		}
		return result, nil
	}
	result.Status, result.Reason = "submission_uncertain", "code_submission_unconfirmed"
	if err := page.settle(ctx); err != nil {
		return result, nil
	}
	if !page.hasSubmissionResponse() {
		return result, nil
	}
	page.clearFields(ctx)
	return verifySubmitted(ctx, page, request, result, true)
}

func validValue(value []byte, limit int) bool {
	return len(value) > 0 && len(value) <= limit && utf8.Valid(value) &&
		strings.TrimSpace(string(value)) != "" && !strings.ContainsAny(string(value), "\r\n\x00")
}

func verifySubmitted(ctx context.Context, page *driver, request Request, result Result, codeAttempted bool) (Result, error) {
	checkChallenge := func(form formState) (Result, error) {
		if !codeAttempted {
			return resolveChallenge(ctx, page, request, form, result)
		}
		result.Status, result.Reason = "requires_challenge", form.Reason
		if form.Status == "supported_challenge" {
			result.Reason = "code_not_verified"
		}
		return result, nil
	}
	if form, err := page.inspect(ctx); err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	} else if isChallenge(form) {
		return checkChallenge(form)
	}
	if err := page.navigate(ctx, request.VerificationURL); err != nil {
		result.Reason = "verification_unavailable"
		return result, nil
	}
	if form, err := page.inspect(ctx); err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	} else if isChallenge(form) {
		return checkChallenge(form)
	}
	verified, err := page.verify(ctx, request.VerificationURL, request.ExpectedText)
	if err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	}
	if verified {
		result.Status, result.Reason = "verified", "verification_matched"
	} else {
		result.Status, result.Reason = "unverified", "verification_not_matched"
	}
	return result, nil
}
