// Package login performs one deterministic password-form login on a retained page.
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

// Run owns and clears the credential bytes returned by credentials. It never
// starts a browser, attaches another connection or sends credentials to a model.
func Run(ctx context.Context, request Request, credentials func(context.Context) (Credentials, error)) (result Result, runErr error) {
	result = Result{Status: "unverified", Reason: "not_attempted"}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	origin, err := browsing.LoginOrigin(request.URL)
	verificationOrigin, verificationErr := browsing.LoginOrigin(request.VerificationURL)
	if err != nil || verificationErr != nil || origin != verificationOrigin || credentials == nil ||
		!utf8.ValidString(request.ExpectedText) || strings.TrimSpace(request.ExpectedText) == "" || len(request.ExpectedText) > 256 || strings.ContainsRune(request.ExpectedText, '\x00') {
		return result, errors.New("login requires same-origin URLs and a bounded non-secret verification marker")
	}
	page, err := newDriver(ctx, origin)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errBrowser
	}
	defer func() {
		if err := page.close(); err != nil && runErr == nil {
			runErr = errBrowser
		}
		if ctx.Err() != nil {
			runErr = ctx.Err()
		}
	}()
	if err := page.navigate(ctx, request.VerificationURL); err != nil {
		result.Reason = "verification_unavailable"
		return result, nil
	}
	if form, err := page.inspect(ctx); err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	} else if form.Status == "requires_challenge" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
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
	if form.Status != "supported" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
	}
	values, err := credentials(ctx)
	defer clear(values.Username)
	defer clear(values.Password)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		return result, errors.New("managed login credentials are unavailable")
	}
	for _, value := range [][]byte{values.Username, values.Password} {
		if len(value) == 0 || len(value) > 1024 || !utf8.Valid(value) || strings.TrimSpace(string(value)) == "" || strings.ContainsAny(string(value), "\r\n\x00") {
			return result, errors.New("managed login credentials are invalid")
		}
	}
	if err := sensitive.Remember(ctx, values.Username, values.Password); err != nil {
		return result, errors.New("login credential protection is unavailable")
	}
	// A command error may follow a successful submission. Never automatically
	// resend this operation, including when its acknowledgement is uncertain.
	result.Status, result.Submitted, result.Reason = "submission_uncertain", true, "submission_unconfirmed"
	form, err = page.submit(ctx, values)
	if err != nil {
		return result, nil
	}
	if form.Status != "submitted" {
		result.Status, result.Submitted, result.Reason = form.Status, form.Submitted, form.Reason
		return result, nil
	}
	if err := page.settle(ctx); err != nil {
		return result, nil
	}
	page.clearFields(ctx)
	if form, err := page.inspect(ctx); err == nil && form.Status == "requires_challenge" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
	}
	if err := page.navigate(ctx, request.VerificationURL); err != nil {
		result.Reason = "verification_unavailable"
		return result, nil
	}
	if form, err := page.inspect(ctx); err != nil {
		result.Reason = "verification_unconfirmed"
		return result, nil
	} else if form.Status == "requires_challenge" {
		result.Status, result.Reason = form.Status, form.Reason
		return result, nil
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
