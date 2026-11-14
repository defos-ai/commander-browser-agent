// Package lightpanda adapts deterministic work to retained browser sessions.
package lightpanda

import (
	"context"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/browser/login"
	"github.com/defos-ai/commander-browser-agent/browser/screenshot"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// Browser adapts deterministic work to the native session's retained connection.
// Its scope never starts a process, owns a second connection or survives the
// manager callback that lent it.
type Browser struct {
	scope hermes.Scope
}

func (*Browser) String() string   { return "scoped Lightpanda browser" }
func (*Browser) GoString() string { return "scoped Lightpanda browser" }

var _ contracts.Browser = (*Browser)(nil)

// NewBrowser binds access to this exact callback without opening a browser.
func NewBrowser(ctx context.Context) (*Browser, error) {
	scope, err := hermes.NewScope(ctx)
	if err != nil {
		return nil, err
	}
	return &Browser{scope: scope}, nil
}

func (b *Browser) Login(ctx context.Context, request contracts.LoginRequest, source contracts.CredentialSource) (contracts.LoginResult, error) {
	if b == nil {
		return contracts.LoginResult{}, hermes.ErrClosed
	}
	if err := b.scope.Check(ctx); err != nil {
		return contracts.LoginResult{}, err
	}
	var credentials func(context.Context) (login.Credentials, error)
	if source.Credentials != nil {
		credentials = func(ctx context.Context) (login.Credentials, error) {
			values, err := source.Credentials(ctx)
			return login.Credentials{Username: values.Username, Password: values.Password}, err
		}
	}
	result, err := login.RunHermes(ctx, login.Request{
		URL: request.URL, VerificationURL: request.VerificationURL, ExpectedText: request.ExpectedText,
		OneTimeCode: source.OneTimeCode,
	}, credentials)
	return contracts.LoginResult{Status: result.Status, Submitted: result.Submitted, Reason: result.Reason}, err
}

func (b *Browser) Screenshot(ctx context.Context, request contracts.ScreenshotRequest) (contracts.Image, error) {
	if b == nil {
		return contracts.Image{}, hermes.ErrClosed
	}
	if err := b.scope.Check(ctx); err != nil {
		return contracts.Image{}, err
	}
	image, err := screenshot.CaptureHermes(ctx, request.Width, request.Height)
	return contracts.Image{PNG: image.PNG, Truncated: image.Truncated}, err
}
